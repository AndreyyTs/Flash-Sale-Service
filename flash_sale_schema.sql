-- =============================================================================
-- FLASH SALE DATABASE SCHEMA
-- Схема базы данных для флеш распродаж
-- =============================================================================

-- Table for storing all checkout requests
-- Таблица для хранения всех checkout запросов
CREATE TABLE IF NOT EXISTS checkouts (
    id BIGSERIAL PRIMARY KEY,                      -- Unique checkout ID / Уникальный ID checkout
    user_id INTEGER NOT NULL,                      -- User who initiated checkout / Пользователь, инициировавший checkout
    item_id INTEGER NOT NULL,                      -- Item being checked out / Товар в процессе покупки
    code UUID UNIQUE NOT NULL,                     -- Unique checkout code / Уникальный код checkout
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),   -- When checkout was created / Время создания checkout
    expires_at TIMESTAMP NOT NULL                  -- When checkout expires / Время истечения checkout
);

-- Performance indexes
-- Индексы для производительности
CREATE INDEX IF NOT EXISTS idx_checkouts_expires_at ON checkouts(expires_at);  -- Index for cleanup queries / Индекс для запросов очистки

-- =============================================================================

-- Table for sale items (lots) for each flash sale
-- Таблица лотов для каждой распродажи
CREATE TABLE sale_items (
    id BIGSERIAL PRIMARY KEY,                      -- Unique item record ID / Уникальный ID записи товара
    sale_id INTEGER NOT NULL,                      -- Sale ID / ID распродажи 
    sale_start_hour TIMESTAMP NOT NULL,            -- Sale start hour / Час начала распродажи
    item_id INTEGER NOT NULL,                      -- Item ID from 0 to 9999 (10000 lots) / ID лота от 0 до 9999 (10000 лотов)
    item_name VARCHAR(255) NOT NULL,               -- Product name / Название товара
    image_url VARCHAR(500) NOT NULL,               -- Image URL / URL картинки
    purchased BOOLEAN NOT NULL DEFAULT FALSE,      -- Purchase status flag / Флаг, куплен ли лот
    purchased_by INTEGER NULL,                     -- User ID who purchased / ID пользователя, кто купил
    purchased_at TIMESTAMP NULL                    -- Purchase timestamp / Время покупки
);

-- Composite index for fast lookups
-- Составной индекс для быстрого поиска
CREATE UNIQUE INDEX IF NOT EXISTS idx_sale_items_sale_item ON sale_items(sale_id, item_id);

-- =============================================================================

-- Stored procedure to create a new sale based on existing data
-- Процедура для создания новой распродажи на основе существующих данных
CREATE OR REPLACE FUNCTION create_new_sale() RETURNS INTEGER AS $$
DECLARE
    max_sale_hour TIMESTAMP;    -- Latest sale hour in database / Последний час распродажи в базе
    max_sale_id INTEGER;        -- Latest sale ID in database / Последний ID распродажи в базе
    new_sale_id INTEGER;        -- New sale ID to create / Новый ID распродажи для создания
    new_sale_hour TIMESTAMP;    -- New sale hour to create / Новый час распродажи для создания
    current_hour TIMESTAMP;     -- Current hour (truncated) / Текущий час (округленный)
    items_generated INTEGER;    -- Number of items created / Количество созданных товаров
BEGIN
    -- Get current hour (truncated to hour)
    -- Получаем текущий час (округленный)
    current_hour := date_trunc('hour', NOW());
    
    -- Find the maximum record in the table
    -- Находим максимальную запись в таблице
    SELECT sale_start_hour, sale_id 
    INTO max_sale_hour, max_sale_id
    FROM sale_items 
    ORDER BY sale_start_hour DESC, sale_id DESC
    LIMIT 1;
    
    -- Logic for creating new sale
    -- Логика создания новой распродажи
    IF max_sale_hour IS NULL THEN
        -- Table is empty - create first sale
        -- Таблица пустая - создаем первую распродажу
        new_sale_id := 1;
        new_sale_hour := current_hour;
        RAISE NOTICE 'Table is empty. Creating first sale with ID % for hour %', new_sale_id, new_sale_hour;
        
    ELSIF max_sale_hour < current_hour THEN
        -- Last sale is older than current hour - create for current hour
        -- Последняя распродажа старше текущего часа - создаем на текущий час
        new_sale_id := max_sale_id + 1;
        new_sale_hour := current_hour;
        RAISE NOTICE 'Last sale % was at %. Creating new sale % for current hour %', 
            max_sale_id, max_sale_hour, new_sale_id, new_sale_hour;
            
    ELSIF max_sale_hour = current_hour THEN
        -- Sale for current hour already exists
        -- Распродажа на текущий час уже существует
        RAISE NOTICE 'Sale % for current hour % already exists. Returning existing sale_id.', 
            max_sale_id, current_hour;
        RETURN max_sale_id;
        
    ELSE
        -- Last sale is in the future - create next sequential sale
        -- Последняя распродажа в будущем - создаем следующую по порядку
        new_sale_id := max_sale_id + 1;
        new_sale_hour := max_sale_hour + INTERVAL '1 hour';
        RAISE NOTICE 'Creating next sequential sale % for hour %', new_sale_id, new_sale_hour;
    END IF;
    
    -- Check if sale with this ID already exists
    -- Проверяем, не существует ли уже распродажа с таким ID
    IF EXISTS (SELECT 1 FROM sale_items WHERE sale_id = new_sale_id LIMIT 1) THEN
        RAISE NOTICE 'Sale with ID % already exists. Returning existing sale_id.', new_sale_id;
        RETURN new_sale_id;
    END IF;
    
    -- Create 10,000 lots for the new sale
    -- Создаем 10,000 лотов для новой распродажи
    INSERT INTO sale_items (
        sale_id,
        sale_start_hour,
        item_id,
        item_name,
        image_url,
        purchased,
        purchased_by,
        purchased_at
    )
    SELECT 
        new_sale_id,                                                                    -- Sale ID / ID распродажи
        new_sale_hour,                                                                  -- Sale hour / Час распродажи
        item_counter,                                                                   -- Item ID (0-9999) / ID товара (0-9999)
        'Flash Item #' || item_counter || ' (Sale ' || new_sale_id || ')',            -- Generated item name / Сгенерированное название товара
        'https://picsum.photos/200/200?random=' || new_sale_id || '_' || item_counter, -- Random image URL / Случайный URL картинки
        false,                                                                          -- Not purchased initially / Изначально не куплен
        NULL,                                                                           -- No purchaser initially / Изначально нет покупателя
        NULL                                                                            -- No purchase time initially / Изначально нет времени покупки
    FROM generate_series(0, 9999) AS item_counter;  -- Generate 10,000 items (0-9999) / Генерируем 10,000 товаров (0-9999)
    
    -- Check number of created records
    -- Проверяем количество созданных записей
    GET DIAGNOSTICS items_generated = ROW_COUNT;
    
    IF items_generated = 10000 THEN
        RAISE NOTICE 'Successfully created sale % with % items for hour %', 
            new_sale_id, items_generated, new_sale_hour;
    ELSE
        RAISE WARNING 'Expected 10000 items but created % for sale %', 
            items_generated, new_sale_id;
    END IF;
    
    RETURN new_sale_id;  -- Return new sale ID / Возвращаем ID новой распродажи
    
EXCEPTION
    WHEN OTHERS THEN
        -- Handle any errors during sale creation
        -- Обрабатываем любые ошибки при создании распродажи
        RAISE EXCEPTION 'Error creating new sale: % (SQLSTATE: %)', SQLERRM, SQLSTATE;
        RETURN NULL; -- Return NULL on error / Возвращаем NULL в случае ошибки
END;
$$ LANGUAGE plpgsql;

-- =============================================================================
-- USAGE EXAMPLES / ПРИМЕРЫ ИСПОЛЬЗОВАНИЯ
-- =============================================================================

-- Set timezone to UTC for consistent timestamps
-- Устанавливаем часовой пояс UTC для согласованности временных меток
SET TIME ZONE 'UTC';

-- Clean up tables (reset everything)
-- Очистка таблиц (сброс всего)
TRUNCATE checkouts RESTART IDENTITY;   -- Clear checkouts table and reset ID counter / Очищаем таблицу checkouts и сбрасываем счетчик ID
TRUNCATE sale_items RESTART IDENTITY;  -- Clear sale_items table and reset ID counter / Очищаем таблицу sale_items и сбрасываем счетчик ID

-- Create a new sale (call the stored procedure)
-- Создание новой распродажи (вызов хранимой процедуры)
SELECT create_new_sale();

-- =============================================================================
-- QUERY EXAMPLES / ПРИМЕРЫ ЗАПРОСОВ
-- =============================================================================

-- View all checkouts
-- Просмотр всех checkouts
SELECT * FROM checkouts c ORDER BY c.created_at DESC;

-- View all sale items (ordered by item_id)
-- Просмотр всех товаров распродажи (упорядочено по item_id)
SELECT * FROM sale_items c ORDER BY c.item_id;

-- View items from specific sale
-- Просмотр товаров конкретной распродажи
SELECT * FROM sale_items WHERE sale_id = 1 ORDER BY item_id;

-- View purchased items only
-- Просмотр только купленных товаров
SELECT * FROM sale_items WHERE purchased = true ORDER BY purchased_at DESC;

-- Count items per sale
-- Подсчет товаров по распродажам
SELECT 
    sale_id,                           -- Sale ID / ID распродажи
    sale_start_hour,                   -- Sale start time / Время начала распродажи
    COUNT(*) as total_items,           -- Total items in sale / Общее количество товаров в распродаже
    COUNT(*) FILTER (WHERE purchased = true) as sold_items,  -- Sold items / Проданные товары
    COUNT(*) FILTER (WHERE purchased = false) as available_items  -- Available items / Доступные товары
FROM sale_items 
GROUP BY sale_id, sale_start_hour 
ORDER BY sale_id;