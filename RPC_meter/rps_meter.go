package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stats holds all test metrics / Статистика хранит все метрики теста
type Stats struct {
	totalRequests      int64
	internalErrors     int64
	successfulRequests int64
	conflictErrors     int64
	otherErrors        int64
	timeouts           int64
	startTime          time.Time
	// Performance metrics / Метрики производительности
	totalLatency int64 // in microseconds / в микросекундах
	maxLatency   int64
	minLatency   int64
	// Purchase flow statistics / Статистика для purchase
	checkoutRequests  int64
	purchaseRequests  int64
	checkoutSuccesses int64
	purchaseSuccesses int64
	checkoutErrors    int64
	purchaseErrors    int64
}

// DataPoint represents chart data point / Структура для точки данных на графике
type DataPoint struct {
	Timestamp time.Time `json:"timestamp"`
	RPS       float64   `json:"rps"`
	Latency   float64   `json:"latency"`
	ErrorRate float64   `json:"errorRate"`
	Success   int64     `json:"success"`
	Errors500 int64     `json:"errors500"`
	// Additional fields for chain testing / Дополнительные поля для цепочки
	CheckoutReqs int64 `json:"checkoutReqs"`
	PurchaseReqs int64 `json:"purchaseReqs"`
	CheckoutSucc int64 `json:"checkoutSucc"`
	PurchaseSucc int64 `json:"purchaseSucc"`
}

// MetricsHistory stores historical data / Структура для хранения исторических данных
type MetricsHistory struct {
	mu     sync.RWMutex
	points []DataPoint
}

// AddPoint adds new data point to history / Добавляет новую точку данных в историю
func (mh *MetricsHistory) AddPoint(point DataPoint) {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	mh.points = append(mh.points, point)

	// Keep only last 300 points (5 minutes at 1 second interval) / Храним только последние 300 точек (5 минут при интервале в 1 секунду)
	if len(mh.points) > 300 {
		mh.points = mh.points[1:]
	}
}

// GetPoints returns copy of all data points / Возвращает копию всех точек данных
func (mh *MetricsHistory) GetPoints() []DataPoint {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	// Return copy / Возвращаем копию
	result := make([]DataPoint, len(mh.points))
	copy(result, mh.points)
	return result
}

// LoadTester main structure for load testing / Основная структура для нагрузочного тестирования
type LoadTester struct {
	baseURL    string
	stats      *Stats
	httpClient *http.Client
	maxUsers   int64 // Maximum number of users / Максимальное количество пользователей
	// Request pool for reuse / Пул для переиспользования запросов
	requestPool sync.Pool
	// Regex for extracting code from checkout response / Regex для извлечения кода из ответа checkout
	codeRegex *regexp.Regexp

	// New fields for charts / Новые поля для графиков
	metricsHistory *MetricsHistory
	webServer      *http.Server
}

// NewLoadTester creates new load tester instance / Создает новый экземпляр нагрузочного тестера
func NewLoadTester(baseURL string, maxUsers int) *LoadTester {
	// HTTP client configuration for high performance / Настройка HTTP-клиента для высокой производительности
	transport := &http.Transport{
		MaxIdleConns:        1000,             // Increase connection pool / Увеличиваем пул соединений
		MaxIdleConnsPerHost: 100,              // More connections per host / Больше соединений на хост
		IdleConnTimeout:     90 * time.Second, // Keep connections longer / Дольше держим соединения
		DisableCompression:  true,             // Disable compression for speed / Отключаем сжатие для скорости
		WriteBufferSize:     32 * 1024,        // Increase buffers / Увеличиваем буферы
		ReadBufferSize:      32 * 1024,
		// TCP configuration / Настройка TCP
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Fast port reuse / Быстрое переиспользование портов
		DisableKeepAlives: false,
		ForceAttemptHTTP2: false, // HTTP/1.1 might be faster for simple requests / HTTP/1.1 может быть быстрее для простых запросов
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second, // Increase timeout for request chains / Увеличиваем таймаут для цепочки запросов
	}

	lt := &LoadTester{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: client,
		maxUsers:   int64(maxUsers),
		stats: &Stats{
			startTime:  time.Now(),
			minLatency: int64(^uint64(0) >> 1), // Maximum int64 value / Максимальное значение int64
		},
		// Compile regex for UUID search in response / Компилируем regex для поиска UUID в ответе
		codeRegex: regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),

		// Initialize new fields / Инициализация новых полей
		metricsHistory: &MetricsHistory{},
	}

	// Initialize request pool / Инициализация пула запросов
	lt.requestPool = sync.Pool{
		New: func() interface{} {
			req, _ := http.NewRequest("POST", "", nil)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("User-Agent", "LoadTester/2.0")
			return req
		},
	}

	return lt
}

// StartWebDashboard starts web server for dashboard / Запуск веб-сервера для дашборда
func (lt *LoadTester) StartWebDashboard(port int) {
	mux := http.NewServeMux()

	// HTML page with charts / HTML страница с графиками
	mux.HandleFunc("/", lt.handleDashboard)
	// API for metrics data / API для получения данных
	mux.HandleFunc("/api/metrics", lt.handleMetricsAPI)

	lt.webServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		fmt.Printf("🌐 Web dashboard available at: http://localhost:%d\n", port)
		if err := lt.webServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Web server startup error: %v\n", err)
		}
	}()
}

// handleDashboard serves dashboard HTML / Обслуживает HTML дашборда
func (lt *LoadTester) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Set correct encoding / Устанавливаем правильную кодировку
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl := `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8"> <!-- REQUIRED -->
    <title>RPS Meter - Dashboard</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>  
    <script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns/dist/chartjs-adapter-date-fns.bundle.min.js"></script>  
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; background: #f5f5f5; }
        .container { max-width: 1400px; margin: 0 auto; }
        .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 20px; margin-bottom: 30px; }
        .stat-card { background: white; padding: 20px; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); text-align: center; }
        .stat-value { font-size: 2.5em; font-weight: bold; color: #2563eb; }
        .stat-label { color: #6b7280; margin-top: 5px; font-size: 0.9em; }
        .charts { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; margin-bottom: 20px; }
        .chart-container { background: white; padding: 20px; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
        .chart-full { grid-column: 1 / -1; }
        /* Fix canvas size */
        .chart-container canvas {
            height: 300px !important;
            width: 100% !important;
        }
        h1 { text-align: center; color: #1f2937; margin-bottom: 30px; }
        h2 { color: #374151; margin-bottom: 15px; font-size: 1.2em; }
        .status-indicator { 
            display: inline-block; 
            width: 12px; 
            height: 12px; 
            border-radius: 50%; 
            margin-right: 8px;
            animation: pulse 2s infinite;
        }
        .status-running { background-color: #10b981; }
        @keyframes pulse {
            0% { opacity: 1; }
            50% { opacity: 0.5; }
            100% { opacity: 1; }
        }
        .test-info {
            background: white;
            padding: 15px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            margin-bottom: 20px;
            text-align: center;
            color: #374151;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🚀 RPS Meter - Real-Time Monitoring</h1>
        <div class="test-info">
            <span class="status-indicator status-running"></span>
            <strong>Test Active</strong> | Updates every second
        </div>
        <div class="stats">
            <div class="stat-card">
                <div class="stat-value" id="currentRPS">0</div>
                <div class="stat-label">Current RPS</div>
            </div>
            <div class="stat-card">
                <div class="stat-value" id="avgLatency">0ms</div>
                <div class="stat-label">Average Latency</div>
            </div>
            <div class="stat-card">
                <div class="stat-value" id="errorRate">0%</div>
                <div class="stat-label">Error Rate</div>
            </div>
            <div class="stat-card">
                <div class="stat-value" id="totalRequests">0</div>
                <div class="stat-label">Total Requests</div>
            </div>
            <div class="stat-card">
                <div class="stat-value" id="successRate">0%</div>
                <div class="stat-label">Success Rate</div>
            </div>
        </div>
        <div class="charts">
            <div class="chart-container">
                <h2>📊 RPS Over Time</h2>
                <canvas id="rpsChart"></canvas>
            </div>
            <div class="chart-container">
                <h2>⏱️ Latency (ms)</h2>
                <canvas id="latencyChart"></canvas>
            </div>
            <div class="chart-container chart-full">
                <h2>📈 Response Distribution</h2>
                <canvas id="statusChart"></canvas>
            </div>
            <div class="chart-container" id="chainChartContainer" style="display: none;">
                <h2>🔗 Chain Steps</h2>
                <canvas id="chainChart"></canvas>
            </div>
        </div>
    </div>
    <script>
        let isChainTest = false;
        const chartConfig = {
            type: 'line',
            options: {
                responsive: true,
                maintainAspectRatio: false,
                scales: {
                    x: {
                        type: 'time',
                        time: {
                            unit: 'second',
                            displayFormats: {
                                second: 'HH:mm:ss'
                            }
                        },
                        title: {
                            display: true,
                            text: 'Time'
                        }
                    },
                    y: {
                        beginAtZero: true
                    }
                },
                elements: {
                    point: {
                        radius: 1
                    },
                    line: {
                        tension: 0.1
                    }
                },
                plugins: {
                    legend: {
                        display: true
                    }
                },
                interaction: {
                    intersect: false,
                    mode: 'index'
                }
            }
        };
        const rpsChart = new Chart(document.getElementById('rpsChart'), {
            ...chartConfig,
            data: {
                datasets: [{
                    label: 'RPS',
                    data: [],
                    borderColor: 'rgb(37, 99, 235)',
                    backgroundColor: 'rgba(37, 99, 235, 0.1)',
                    fill: true
                }]
            }
        });
        const latencyChart = new Chart(document.getElementById('latencyChart'), {
            ...chartConfig,
            data: {
                datasets: [{
                    label: 'Latency (ms)',
                    data: [],
                    borderColor: 'rgb(16, 185, 129)',
                    backgroundColor: 'rgba(16, 185, 129, 0.1)',
                    fill: true
                }]
            }
        });
        const statusChart = new Chart(document.getElementById('statusChart'), {
            ...chartConfig,
            data: {
                datasets: [
                    {
                        label: '✅ Success (200 + 409)',
                        data: [],
                        borderColor: 'rgb(34, 197, 94)',
                        backgroundColor: 'rgba(34, 197, 94, 0.1)',
                        fill: false
                    },
                    {
                        label: '❌ Server Errors (500)',
                        data: [],
                        borderColor: 'rgb(239, 68, 68)',
                        backgroundColor: 'rgba(239, 68, 68, 0.1)',
                        fill: false
                    },
                ]
            }
        });
        const chainChart = new Chart(document.getElementById('chainChart'), {
            ...chartConfig,
            data: {
                datasets: [
                    {
                        label: 'Checkout Requests',
                        data: [],
                        borderColor: 'rgb(99, 102, 241)',
                        backgroundColor: 'rgba(99, 102, 241, 0.1)',
                        fill: false
                    },
                    {
                        label: 'Checkout Success',
                        data: [],
                        borderColor: 'rgb(34, 197, 94)',
                        backgroundColor: 'rgba(34, 197, 94, 0.1)',
                        fill: false
                    },
                    {
                        label: 'Purchase Requests',
                        data: [],
                        borderColor: 'rgb(168, 85, 247)',
                        backgroundColor: 'rgba(168, 85, 247, 0.1)',
                        fill: false
                    },
                    {
                        label: 'Purchase Success',
                        data: [],
                        borderColor: 'rgb(59, 130, 246)',
                        backgroundColor: 'rgba(59, 130, 246, 0.1)',
                        fill: false
                    }
                ]
            }
        });
        function adjustChartContainers() {
            document.querySelectorAll('.chart-container canvas').forEach(canvas => {
                canvas.style.height = '300px';
            });
        }
        async function updateCharts() {
            try {
                const response = await fetch('/api/metrics');
                const data = await response.json();
                if (data.length === 0) return;
                const latest = data[data.length - 1];
                if (latest.checkoutReqs > 0 && !isChainTest) {
                    isChainTest = true;
                    document.getElementById('chainChartContainer').style.display = 'block';
                    document.querySelector('.charts').style.gridTemplateColumns = '1fr 1fr';
                }
                document.getElementById('currentRPS').textContent = Math.round(latest.rps);
                document.getElementById('avgLatency').textContent = Math.round(latest.latency) + 'ms';
                document.getElementById('errorRate').textContent = Math.round(latest.errorRate) + '%';
                const totalReqs = latest.success + latest.errors500;
                document.getElementById('totalRequests').textContent = totalReqs.toLocaleString();
                const successRate = totalReqs > 0 ? (latest.success / totalReqs * 100) : 0;
                document.getElementById('successRate').textContent = Math.round(successRate) + '%';
                rpsChart.data.datasets[0].data = data.map(point => ({
                    x: new Date(point.timestamp),
                    y: point.rps
                }));
                latencyChart.data.datasets[0].data = data.map(point => ({
                    x: new Date(point.timestamp),
                    y: point.latency
                }));
                statusChart.data.datasets[0].data = data.map(point => ({
                    x: new Date(point.timestamp),
                    y: point.success
                }));
                statusChart.data.datasets[1].data = data.map(point => ({
                    x: new Date(point.timestamp),
                    y: point.errors500
                }));
                if (isChainTest) {
                    chainChart.data.datasets[0].data = data.map(point => ({
                        x: new Date(point.timestamp),
                        y: point.checkoutReqs
                    }));
                    chainChart.data.datasets[1].data = data.map(point => ({
                        x: new Date(point.timestamp),
                        y: point.checkoutSucc
                    }));
                    chainChart.data.datasets[2].data = data.map(point => ({
                        x: new Date(point.timestamp),
                        y: point.purchaseReqs
                    }));
                    chainChart.data.datasets[3].data = data.map(point => ({
                        x: new Date(point.timestamp),
                        y: point.purchaseSucc
                    }));
                    chainChart.update('none');
                }
                rpsChart.update('none');
                latencyChart.update('none');
                statusChart.update('none');
            } catch (error) {
                console.error('Error fetching data:', error);
            }
        }
        adjustChartContainers();
        updateCharts();
        setInterval(updateCharts, 1000);
        window.addEventListener('resize', adjustChartContainers);
    </script>
</body>
</html>`
	fmt.Fprint(w, tmpl)
}

// handleMetricsAPI serves metrics data as JSON / Обслуживает данные метрик в формате JSON
func (lt *LoadTester) handleMetricsAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	points := lt.metricsHistory.GetPoints()
	json.NewEncoder(w).Encode(points)
}

// collectMetrics gathers and stores current metrics / Метод сбора метрик
func (lt *LoadTester) collectMetrics() {
	elapsed := time.Since(lt.stats.startTime).Seconds()
	total := atomic.LoadInt64(&lt.stats.totalRequests)
	errors500 := atomic.LoadInt64(&lt.stats.internalErrors)
	successful := atomic.LoadInt64(&lt.stats.successfulRequests) + atomic.LoadInt64(&lt.stats.conflictErrors)
	totalLatency := atomic.LoadInt64(&lt.stats.totalLatency)

	// Chain metrics / Метрики для цепочки
	checkoutReqs := atomic.LoadInt64(&lt.stats.checkoutRequests)
	purchaseReqs := atomic.LoadInt64(&lt.stats.purchaseRequests)
	checkoutSucc := atomic.LoadInt64(&lt.stats.checkoutSuccesses)
	purchaseSucc := atomic.LoadInt64(&lt.stats.purchaseSuccesses)

	currentRPS := float64(total) / elapsed
	if elapsed < 1 {
		currentRPS = float64(total) // for first second / для первой секунды
	}

	errorRate := float64(0)
	if total > 0 {
		errorRate = float64(errors500) / float64(total) * 100
	}

	avgLatency := float64(0)
	if total > 0 {
		avgLatency = float64(totalLatency) / float64(total) / 1000
	}

	// Add point to history / Добавляем точку в историю
	point := DataPoint{
		Timestamp:    time.Now(),
		RPS:          currentRPS,
		Latency:      avgLatency,
		ErrorRate:    errorRate,
		Success:      successful,
		Errors500:    errors500,
		CheckoutReqs: checkoutReqs,
		PurchaseReqs: purchaseReqs,
		CheckoutSucc: checkoutSucc,
		PurchaseSucc: purchaseSucc,
	}

	lt.metricsHistory.AddPoint(point)
}

// generateRequest creates random user and item IDs / Генерирует случайные ID пользователя и товара
func (lt *LoadTester) generateRequest() (int64, int64) {
	// IMPORTANT: Using logic from old working code / ВАЖНО: Используем логику из старого рабочего кода
	// If maxUsers = 0 or very large, use 1_000_000 as in old code / Если maxUsers = 0 или очень большое, используем 1_000_000 как в старом коде
	var userID int64
	if lt.maxUsers <= 0 || lt.maxUsers > 1_000_000 {
		userID = rand.Int63n(1_000_000) // as in old code / как в старом коде
	} else {
		userID = rand.Int63n(lt.maxUsers) // from 0 to maxUsers-1 / от 0 до maxUsers-1
	}

	itemID := rand.Int63n(10000) // as in old code / как в старом коде
	return userID, itemID
}

// makeRequest performs single checkout request / Старый метод для тестирования только checkout
func (lt *LoadTester) makeRequest(userID, itemID int64) {
	start := time.Now()

	// Get request from pool / Получаем запрос из пула
	req := lt.requestPool.Get().(*http.Request)
	defer lt.requestPool.Put(req)

	// Update URL / Обновляем URL
	req.URL, _ = req.URL.Parse(fmt.Sprintf("%s/checkout?user_id=%d&item_id=%d", lt.baseURL, userID, itemID))

	resp, err := lt.httpClient.Do(req)
	if err != nil {
		atomic.AddInt64(&lt.stats.otherErrors, 1)
		atomic.AddInt64(&lt.stats.totalRequests, 1)
		return
	}

	// Read and close response body as fast as possible / Читаем и закрываем тело ответа максимально быстро
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Calculate latency / Вычисляем латентность
	latency := time.Since(start).Microseconds()
	atomic.AddInt64(&lt.stats.totalLatency, latency)

	// Update min/max latency / Обновляем мин/макс латентность
	for {
		current := atomic.LoadInt64(&lt.stats.maxLatency)
		if latency <= current || atomic.CompareAndSwapInt64(&lt.stats.maxLatency, current, latency) {
			break
		}
	}

	for {
		current := atomic.LoadInt64(&lt.stats.minLatency)
		if latency >= current || atomic.CompareAndSwapInt64(&lt.stats.minLatency, current, latency) {
			break
		}
	}

	atomic.AddInt64(&lt.stats.totalRequests, 1)

	switch resp.StatusCode {
	case http.StatusOK:
		atomic.AddInt64(&lt.stats.successfulRequests, 1)
	case http.StatusInternalServerError:
		atomic.AddInt64(&lt.stats.internalErrors, 1)
	case http.StatusConflict:
		atomic.AddInt64(&lt.stats.conflictErrors, 1)
	default:
		atomic.AddInt64(&lt.stats.otherErrors, 1)
	}
}

// makeChainedRequest performs checkout->purchase chain / Новый метод для тестирования цепочки checkout -> purchase
func (lt *LoadTester) makeChainedRequest(userID, itemID int64) {
	start := time.Now()

	// Step 1: make checkout / Этап 1: делаем checkout
	checkoutReq := lt.requestPool.Get().(*http.Request)
	defer lt.requestPool.Put(checkoutReq)

	checkoutReq.URL, _ = checkoutReq.URL.Parse(fmt.Sprintf("%s/checkout?user_id=%d&item_id=%d", lt.baseURL, userID, itemID))

	atomic.AddInt64(&lt.stats.checkoutRequests, 1)

	checkoutResp, err := lt.httpClient.Do(checkoutReq)
	if err != nil {
		atomic.AddInt64(&lt.stats.checkoutErrors, 1)
		atomic.AddInt64(&lt.stats.otherErrors, 1)
		atomic.AddInt64(&lt.stats.totalRequests, 1)
		return
	}

	// Read checkout response body / Читаем тело ответа checkout
	checkoutBody, err := io.ReadAll(checkoutResp.Body)
	checkoutResp.Body.Close()

	if err != nil || checkoutResp.StatusCode != http.StatusOK {
		atomic.AddInt64(&lt.stats.checkoutErrors, 1)
		atomic.AddInt64(&lt.stats.totalRequests, 1)

		switch checkoutResp.StatusCode {
		case http.StatusInternalServerError:
			atomic.AddInt64(&lt.stats.internalErrors, 1)
		case http.StatusConflict:
			atomic.AddInt64(&lt.stats.conflictErrors, 1)
		default:
			atomic.AddInt64(&lt.stats.otherErrors, 1)
		}
		return
	}

	atomic.AddInt64(&lt.stats.checkoutSuccesses, 1)

	// Extract code from checkout response / Извлекаем код из ответа checkout
	// IMPORTANT: First trim whitespace, as in debug version / ВАЖНО: Сначала очищаем от пробелов, как в отладочной версии
	code := strings.TrimSpace(string(checkoutBody))

	// If code is empty after trimming, try to find UUID with regex / Если после очистки код пустой, пробуем найти UUID регуляркой
	if code == "" || !strings.Contains(code, "-") {
		code = lt.codeRegex.FindString(string(checkoutBody))
	}

	if code == "" {
		atomic.AddInt64(&lt.stats.checkoutErrors, 1)
		atomic.AddInt64(&lt.stats.otherErrors, 1)
		atomic.AddInt64(&lt.stats.totalRequests, 1)
		return
	}

	// Step 2: make purchase / Этап 2: делаем purchase
	purchaseReq := lt.requestPool.Get().(*http.Request)
	defer lt.requestPool.Put(purchaseReq)

	purchaseReq.URL, _ = purchaseReq.URL.Parse(fmt.Sprintf("%s/purchase?code=%s", lt.baseURL, code))

	atomic.AddInt64(&lt.stats.purchaseRequests, 1)

	purchaseResp, err := lt.httpClient.Do(purchaseReq)
	if err != nil {
		atomic.AddInt64(&lt.stats.purchaseErrors, 1)
		atomic.AddInt64(&lt.stats.otherErrors, 1)
		atomic.AddInt64(&lt.stats.totalRequests, 1)
		return
	}

	// Read and close purchase response body / Читаем и закрываем тело ответа purchase
	io.Copy(io.Discard, purchaseResp.Body)
	purchaseResp.Body.Close()

	// Calculate total chain latency / Вычисляем общую латентность цепочки
	latency := time.Since(start).Microseconds()
	atomic.AddInt64(&lt.stats.totalLatency, latency)

	// Update min/max latency / Обновляем мин/макс латентность
	for {
		current := atomic.LoadInt64(&lt.stats.maxLatency)
		if latency <= current || atomic.CompareAndSwapInt64(&lt.stats.maxLatency, current, latency) {
			break
		}
	}

	for {
		current := atomic.LoadInt64(&lt.stats.minLatency)
		if latency >= current || atomic.CompareAndSwapInt64(&lt.stats.minLatency, current, latency) {
			break
		}
	}

	atomic.AddInt64(&lt.stats.totalRequests, 1)

	// Process purchase result / Обрабатываем результат purchase
	switch purchaseResp.StatusCode {
	case http.StatusOK:
		atomic.AddInt64(&lt.stats.purchaseSuccesses, 1)
		atomic.AddInt64(&lt.stats.successfulRequests, 1)
	case http.StatusInternalServerError:
		atomic.AddInt64(&lt.stats.purchaseErrors, 1)
		atomic.AddInt64(&lt.stats.internalErrors, 1)
	case http.StatusConflict:
		atomic.AddInt64(&lt.stats.purchaseErrors, 1)
		atomic.AddInt64(&lt.stats.conflictErrors, 1)
	default:
		atomic.AddInt64(&lt.stats.purchaseErrors, 1)
		atomic.AddInt64(&lt.stats.otherErrors, 1)
	}
}

// worker performs load testing with support for different test types / Улучшенный воркер с поддержкой разных типов тестов
func (lt *LoadTester) worker(ctx context.Context, requestsPerSecond int, wg *sync.WaitGroup, testChain bool) {
	defer wg.Done()

	// Calculate interval between requests / Вычисляем интервал между запросами
	interval := time.Second / time.Duration(requestsPerSecond)

	// Use batch processing for very high RPS / Для очень высоких RPS используем пакетную обработку
	batchSize := 1
	if requestsPerSecond > 1000 {
		batchSize = requestsPerSecond / 1000
		if batchSize > 100 {
			batchSize = 100
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Send batch of requests / Отправляем пакет запросов
			for i := 0; i < batchSize; i++ {
				userID, itemID := lt.generateRequest()
				if testChain {
					go lt.makeChainedRequest(userID, itemID)
				} else {
					go lt.makeRequest(userID, itemID)
				}
			}
		}
	}
}

// RunLoadTest starts the main load testing process / Запускает основной процесс нагрузочного тестирования
func (lt *LoadTester) RunLoadTest(rps int, duration time.Duration, numWorkers int, testChain bool) {
	testType := "checkout"
	if testChain {
		testType = "checkout->purchase chain"
	}

	if !lt.TestSingleRequest(testChain) {
		fmt.Printf("Testing stopped due to server issues\n")
		return
	}

	// Start web dashboard on port 9090 / Запускаем веб-дашборд на порту 9090
	lt.StartWebDashboard(9090)

	// High performance configuration / Настройка для высокой производительности
	runtime.GOMAXPROCS(runtime.NumCPU())

	fmt.Printf("Starting high-performance load testing (%s):\n", testType)
	fmt.Printf("- Target RPS: %d\n", rps)
	fmt.Printf("- Duration: %v\n", duration)
	fmt.Printf("- Number of workers: %d\n", numWorkers)
	fmt.Printf("- Number of users: %d\n", lt.maxUsers)
	fmt.Printf("- RPS per worker: %.1f\n", float64(rps)/float64(numWorkers))
	fmt.Printf("- CPU cores: %d\n", runtime.NumCPU())
	fmt.Printf("- URL: %s\n", lt.baseURL)
	fmt.Printf("- Web dashboard: http://localhost:9090\n\n")

	// Reset statistics / Сброс статистики
	lt.stats = &Stats{
		startTime:  time.Now(),
		minLatency: int64(^uint64(0) >> 1),
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	requestsPerWorker := rps / numWorkers

	// Start workers / Запускаем воркеры
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go lt.worker(ctx, requestsPerWorker, &wg, testChain)
	}

	// Statistics in separate goroutine / Статистика в отдельной горутине
	go lt.printStatsLoop(ctx, testChain)

	wg.Wait()
	time.Sleep(1 * time.Second) // Give time to finish last requests / Даем время завершить последние запросы
	lt.printFinalStats(testChain)

	fmt.Printf("\n🌐 Web dashboard continues running at http://localhost:9090\n")
	fmt.Printf("Press Ctrl+C to exit the program\n")

	// Wait for termination signal / Ждем сигнал завершения
	select {}
}

// printStatsLoop prints statistics periodically / Выводит статистику периодически
func (lt *LoadTester) printStatsLoop(ctx context.Context, testChain bool) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lt.collectMetrics()             // First collect metrics for charts / Сначала собираем метрики для графиков
			lt.printCurrentStats(testChain) // Then print to console / Потом выводим в консоль
		}
	}
}

// printCurrentStats displays current test statistics / Выводит текущую статистику
func (lt *LoadTester) printCurrentStats(testChain bool) {
	elapsed := time.Since(lt.stats.startTime).Seconds()
	total := atomic.LoadInt64(&lt.stats.totalRequests)
	errors500 := atomic.LoadInt64(&lt.stats.internalErrors)
	conflicts := atomic.LoadInt64(&lt.stats.conflictErrors)
	successful := atomic.LoadInt64(&lt.stats.successfulRequests) + atomic.LoadInt64(&lt.stats.conflictErrors)
	otherErrors := atomic.LoadInt64(&lt.stats.otherErrors)
	totalLatency := atomic.LoadInt64(&lt.stats.totalLatency)

	currentRPS := float64(total) / elapsed
	errorRate := float64(errors500) / float64(total) * 100
	conflictRate := float64(conflicts) / float64(total) * 100
	otherErrorsRate := float64(otherErrors) / float64(total) * 100
	avgLatency := float64(0)

	if total > 0 {
		avgLatency = float64(totalLatency) / float64(total) / 1000 // in milliseconds / в миллисекундах
	} else {
		errorRate = 0
		conflictRate = 0
	}

	if testChain {
		checkoutReqs := atomic.LoadInt64(&lt.stats.checkoutRequests)
		purchaseReqs := atomic.LoadInt64(&lt.stats.purchaseRequests)
		checkoutSucc := atomic.LoadInt64(&lt.stats.checkoutSuccesses)
		purchaseSucc := atomic.LoadInt64(&lt.stats.purchaseSuccesses)

		fmt.Printf("[%.1fs] RPS: %.0f | Total: %d | Checkout: %d->%d | Purchase: %d->%d | 500: %d (%.1f%%) | 409: %d (%.1f%%) | Avg Latency: %.2fms\n",
			elapsed, currentRPS, total, checkoutReqs, checkoutSucc, purchaseReqs, purchaseSucc, errors500, errorRate, conflicts, conflictRate, avgLatency)
	} else {
		fmt.Printf("[%.1fs] RPS: %.0f | Total: %d | 200: %d | 500: %d (%.1f%%) | 409: %d (%.1f%%) | Other: %d (%.1f%%) | Avg Latency: %.2fms\n",
			elapsed, currentRPS, total, successful, errors500, errorRate, conflicts, conflictRate, otherErrors, otherErrorsRate, avgLatency)
	}
}

// printFinalStats displays final test results / Выводит финальную статистику
func (lt *LoadTester) printFinalStats(testChain bool) {
	elapsed := time.Since(lt.stats.startTime).Seconds()
	total := atomic.LoadInt64(&lt.stats.totalRequests)
	errors500 := atomic.LoadInt64(&lt.stats.internalErrors)
	conflicts := atomic.LoadInt64(&lt.stats.conflictErrors)
	successful := atomic.LoadInt64(&lt.stats.successfulRequests)
	otherErrors := atomic.LoadInt64(&lt.stats.otherErrors)
	totalLatency := atomic.LoadInt64(&lt.stats.totalLatency)
	maxLatency := atomic.LoadInt64(&lt.stats.maxLatency)
	minLatency := atomic.LoadInt64(&lt.stats.minLatency)

	avgRPS := float64(total) / elapsed
	errorRate := float64(errors500) / float64(total) * 100
	conflictRate := float64(conflicts) / float64(total) * 100
	successRate := float64(successful) / float64(total) * 100
	avgLatency := float64(0)

	if total > 0 {
		avgLatency = float64(totalLatency) / float64(total) / 1000
	} else {
		errorRate = 0
		conflictRate = 0
		successRate = 0
	}

	testTypeStr := "CHECKOUT"
	if testChain {
		testTypeStr = "CHECKOUT->PURCHASE CHAIN"
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 80))
	fmt.Printf("FINAL LOAD TESTING STATISTICS %s\n", testTypeStr)
	fmt.Printf("%s\n", strings.Repeat("=", 80))
	fmt.Printf("Total testing time: %.2f seconds\n", elapsed)
	fmt.Printf("Total requests: %d\n", total)
	fmt.Printf("Achieved RPS: %.0f\n", avgRPS)
	fmt.Printf("Users: %d\n", lt.maxUsers)
	fmt.Printf("\nPerformance:\n")
	fmt.Printf("- Average latency: %.2f ms\n", avgLatency)
	fmt.Printf("- Minimum latency: %.2f ms\n", float64(minLatency)/1000)
	fmt.Printf("- Maximum latency: %.2f ms\n", float64(maxLatency)/1000)

	if testChain {
		checkoutReqs := atomic.LoadInt64(&lt.stats.checkoutRequests)
		purchaseReqs := atomic.LoadInt64(&lt.stats.purchaseRequests)
		checkoutSucc := atomic.LoadInt64(&lt.stats.checkoutSuccesses)
		purchaseSucc := atomic.LoadInt64(&lt.stats.purchaseSuccesses)
		checkoutErrors := atomic.LoadInt64(&lt.stats.checkoutErrors)
		purchaseErrors := atomic.LoadInt64(&lt.stats.purchaseErrors)

		fmt.Printf("\nStep breakdown:\n")
		fmt.Printf("- Checkout requests: %d\n", checkoutReqs)
		fmt.Printf("- Checkout successful: %d (%.2f%%)\n", checkoutSucc, float64(checkoutSucc)/float64(checkoutReqs)*100)
		fmt.Printf("- Checkout errors: %d (%.2f%%)\n", checkoutErrors, float64(checkoutErrors)/float64(checkoutReqs)*100)
		fmt.Printf("- Purchase requests: %d\n", purchaseReqs)
		fmt.Printf("- Purchase successful: %d (%.2f%%)\n", purchaseSucc, float64(purchaseSucc)/float64(purchaseReqs)*100)
		fmt.Printf("- Purchase errors: %d (%.2f%%)\n", purchaseErrors, float64(purchaseErrors)/float64(purchaseReqs)*100)
	}

	fmt.Printf("\nFinal response distribution:\n")
	fmt.Printf("- 200 OK: %d (%.2f%%)\n", successful, successRate)
	fmt.Printf("- 500 Internal Server Error: %d (%.2f%%)\n", errors500, errorRate)
	fmt.Printf("- 409 Conflict: %d (%.2f%%)\n", conflicts, conflictRate)
	fmt.Printf("- Other errors/timeouts: %d (%.2f%%)\n", otherErrors, float64(otherErrors)/float64(total)*100)
	fmt.Printf("%s\n", strings.Repeat("=", 80))
}

// TestSingleRequest tests server availability / Тестирует доступность сервера
func (lt *LoadTester) TestSingleRequest(testChain bool) bool {
	if testChain {
		fmt.Printf("Checking server availability (checkout->purchase chain)...\n")
		return lt.testChainedRequest()
	} else {
		fmt.Printf("Checking server availability (checkout)...\n")
		return lt.testCheckoutOnly()
	}
}

// testCheckoutOnly tests single checkout request / Тестирует одиночный checkout запрос
func (lt *LoadTester) testCheckoutOnly() bool {
	userID, itemID := lt.generateRequest()
	url := fmt.Sprintf("%s/checkout?user_id=%d&item_id=%d", lt.baseURL, userID, itemID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		fmt.Printf("❌ Request creation error: %v\n", err)
		return false
	}

	resp, err := lt.httpClient.Do(req)
	if err != nil {
		fmt.Printf("❌ Request execution error: %v\n", err)
		return false
	}
	defer resp.Body.Close()

	io.Copy(io.Discard, resp.Body)

	fmt.Printf("✅ Status: %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusConflict, http.StatusInternalServerError:
		fmt.Printf("✅ Server is available for testing!\n\n")
		return true
	default:
		fmt.Printf("⚠️  Unexpected status, but continuing...\n\n")
		return true
	}
}

// testChainedRequest tests checkout->purchase chain / Тестирует цепочку checkout->purchase
func (lt *LoadTester) testChainedRequest() bool {
	userID, itemID := lt.generateRequest()

	// Test checkout / Тест checkout
	checkoutURL := fmt.Sprintf("%s/checkout?user_id=%d&item_id=%d", lt.baseURL, userID, itemID)
	fmt.Printf("🔍 Checkout URL: %s\n", checkoutURL)

	checkoutReq, err := http.NewRequest("POST", checkoutURL, nil)
	if err != nil {
		fmt.Printf("❌ Checkout request creation error: %v\n", err)
		return false
	}

	checkoutResp, err := lt.httpClient.Do(checkoutReq)
	if err != nil {
		fmt.Printf("❌ Checkout request execution error: %v\n", err)
		return false
	}

	checkoutBody, err := io.ReadAll(checkoutResp.Body)
	checkoutResp.Body.Close()

	fmt.Printf("✅ Checkout status: %d %s\n", checkoutResp.StatusCode, http.StatusText(checkoutResp.StatusCode))

	if checkoutResp.StatusCode != http.StatusOK {
		fmt.Printf("⚠️  Checkout didn't return 200, testing checkout only...\n")
		fmt.Printf("📄 Server response: %s\n\n", string(checkoutBody))
		return true
	}

	// Show raw response for debugging / Показываем сырой ответ для отладки
	fmt.Printf("📄 Raw checkout response: [%s]\n", string(checkoutBody))
	fmt.Printf("📏 Response length: %d bytes\n", len(checkoutBody))

	// Trim whitespace / Очищаем от пробелов
	code := strings.TrimSpace(string(checkoutBody))
	fmt.Printf("🔤 Cleaned code: [%s]\n", code)

	// If no dashes, try to find UUID with regex / Если нет дефисов, пробуем найти UUID регуляркой
	if !strings.Contains(code, "-") {
		fmt.Printf("⚠️  Code doesn't contain dashes, searching for UUID with regex...\n")
		code = lt.codeRegex.FindString(string(checkoutBody))
	}

	if code == "" {
		fmt.Printf("❌ Failed to extract code from checkout response\n")
		return false
	}

	fmt.Printf("✅ Got code: %s\n", code)

	// Test purchase / Тест purchase
	purchaseURL := fmt.Sprintf("%s/purchase?code=%s", lt.baseURL, code)
	fmt.Printf("🔍 Purchase URL: %s\n", purchaseURL)

	purchaseReq, err := http.NewRequest("POST", purchaseURL, nil)
	if err != nil {
		fmt.Printf("❌ Purchase request creation error: %v\n", err)
		return false
	}

	purchaseResp, err := lt.httpClient.Do(purchaseReq)
	if err != nil {
		fmt.Printf("❌ Purchase request execution error: %v\n", err)
		return false
	}

	purchaseBody, err := io.ReadAll(purchaseResp.Body)
	purchaseResp.Body.Close()

	fmt.Printf("✅ Purchase status: %d %s\n", purchaseResp.StatusCode, http.StatusText(purchaseResp.StatusCode))

	if purchaseResp.StatusCode != http.StatusOK {
		fmt.Printf("📄 Purchase response: %s\n", string(purchaseBody))
	}

	fmt.Printf("✅ Chain tested successfully!\n\n")

	return true
}

// parseDuration parses duration string / Парсит строку длительности
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "s") {
		return time.ParseDuration(s)
	}
	if strings.HasSuffix(s, "m") {
		return time.ParseDuration(s)
	}
	if strings.HasSuffix(s, "h") {
		return time.ParseDuration(s)
	}
	// If no suffix, assume seconds / Если нет суффикса, считаем что это секунды
	return time.ParseDuration(s + "s")
}

// printUsage displays help information / Выводит справочную информацию
func printUsage() {
	fmt.Printf("Usage: %s [options]\n\n", "rps_meter")
	fmt.Printf("Options:\n")
	fmt.Printf("  -rps int        Target RPS (requests per second) (default: 1000)\n")
	fmt.Printf("  -users int      Number of users (default: 100)\n")
	fmt.Printf("  -duration string Test duration (e.g.: 30s, 1m, 2h) (default: 60s)\n")
	fmt.Printf("  -url string     Server URL (default: http://localhost:8080)\n")
	fmt.Printf("  -chain bool     Test checkout->purchase chain (default: false)\n")
	fmt.Printf("  -workers int    Number of workers (default: automatic)\n")
	fmt.Printf("  -help           Show this help\n\n")
	fmt.Printf("Web Dashboard:\n")
	fmt.Printf("  Automatically starts at http://localhost:9090\n")
	fmt.Printf("  Shows real-time charts\n\n")
	fmt.Printf("Examples:\n")
	fmt.Printf("  # Basic checkout test with 1000 RPS\n")
	fmt.Printf("  %s -rps=1000 -duration=1m\n\n", "rps_meter")
	fmt.Printf("  # Test checkout->purchase chain\n")
	fmt.Printf("  %s -rps=5000 -duration=2m -chain=true\n\n", "rps_meter")
	fmt.Printf("  # Test with limited number of users\n")
	fmt.Printf("  %s -rps=100 -users=100 -duration=30s\n\n", "rps_meter")
}

func main() {
	// Command line flags definition / Определение флагов командной строки
	var (
		rps      = flag.Int("rps", 1000, "Target RPS (requests per second)")
		users    = flag.Int("users", 100, "Number of users")
		duration = flag.String("duration", "60s", "Test duration (e.g.: 30s, 1m, 2h)")
		baseURL  = flag.String("url", "http://localhost:8080", "Server URL")
		chain    = flag.Bool("chain", false, "Test checkout->purchase chain")
		workers  = flag.Int("workers", 0, "Number of workers (0 = automatic)")
		help     = flag.Bool("help", false, "Show help")
	)

	flag.Parse()

	// Show help / Показать справку
	if *help {
		printUsage()
		return
	}

	// Parameter validation / Валидация параметров
	if *rps <= 0 {
		fmt.Printf("❌ Error: RPS must be greater than 0\n")
		return
	}

	if *users <= 0 {
		fmt.Printf("❌ Error: Number of users must be greater than 0\n")
		return
	}

	// Duration parsing / Парсинг длительности
	testDuration, err := parseDuration(*duration)
	if err != nil {
		fmt.Printf("❌ Duration parsing error '%s': %v\n", *duration, err)
		fmt.Printf("Valid examples: 30s, 1m, 2h\n")
		return
	}

	// Automatic worker count calculation / Автоматический расчет количества воркеров
	numWorkers := *workers
	if numWorkers == 0 {
		numWorkers = *rps / 10 // 10 RPS per worker by default / 10 RPS на воркера по умолчанию
		if numWorkers < 10 {
			numWorkers = 10
		}
		if numWorkers > 1000 {
			numWorkers = 1000
		}
	}

	// Sanity check parameters / Проверка разумности параметров
	if *rps > 100000 {
		fmt.Printf("⚠️  Warning: Very high RPS (%d). Make sure your system can handle this.\n", *rps)
	}

	// Configuration output / Вывод конфигурации
	fmt.Printf("🚀 RPS Meter - Load Testing\n")
	fmt.Printf("%s\n", strings.Repeat("=", 50))
	fmt.Printf("Test configuration:\n")
	fmt.Printf("- Target RPS: %d\n", *rps)
	fmt.Printf("- Users: %d\n", *users)
	fmt.Printf("- Duration: %v\n", testDuration)
	fmt.Printf("- URL: %s\n", *baseURL)
	fmt.Printf("- Test type: ")
	if *chain {
		fmt.Printf("checkout->purchase chain\n")
	} else {
		fmt.Printf("checkout only\n")
	}
	fmt.Printf("- Workers: %d\n", numWorkers)
	fmt.Printf("- Web dashboard: http://localhost:9090\n")
	fmt.Printf("%s\n\n", strings.Repeat("=", 50))

	// Create tester / Создание тестера
	tester := NewLoadTester(*baseURL, *users)

	// Run test / Запуск теста
	tester.RunLoadTest(*rps, testDuration, numWorkers, *chain)
}
