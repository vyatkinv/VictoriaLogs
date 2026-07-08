# ARCHITECTURE.md

Гайд по навигации и погружению в кодовую базу VictoriaLogs.

---

## Содержание

1. [Структура репозитория и точки входа](#1-структура-репозитория-и-точки-входа)
2. [HTTP-слой: vlinsert / vlselect / vlstorage](#2-http-слой)
3. [Физическая структура хранилища на диске](#3-физическая-структура-хранилища)
4. [Движок хранилища: Storage → Partition → datadb → Part → Block](#4-движок-хранилища)
5. [Путь записи (ingestion path)](#5-путь-записи-ingestion-path)
6. [Путь чтения (query path)](#6-путь-чтения-query-path)
7. [LogsQL: парсер, фильтры, пайпы](#7-logsql-парсер-фильтры-пайпы)
8. [Ключевые алгоритмы и подходы](#8-ключевые-алгоритмы-и-подходы)
9. [Кластерный режим](#9-кластерный-режим)
10. [Паттерны управления памятью](#10-паттерны-управления-памятью)
11. [Что хранится в памяти и как данные попадают на диск](#11-что-хранится-в-памяти-и-как-данные-попадают-на-диск)
12. [Бинарный формат файлов в Part](#12-бинарный-формат-файлов-в-part)

---

## 1. Структура репозитория и точки входа

```
app/
  victoria-logs/   ← точка входа: single-node binary (main.go)
  vlinsert/        ← HTTP-хендлеры для всех протоколов записи
  vlselect/        ← HTTP-хендлеры для запросов LogsQL
  vlstorage/       ← обёртка над хранилищем + конфигурация через флаги
  vlagent/         ← агент сбора логов (файлы + Kubernetes)
  vlogscli/        ← интерактивный CLI для запросов
  vlogsgenerator/  ← генератор синтетической нагрузки для тестов
lib/
  logstorage/      ← ВЕСЬ движок: хранилище, LogsQL, парсер, пайпы (~340 файлов)
  prefixfilter/    ← утилита allow/deny-фильтрации по префиксам
apptest/           ← интеграционные тесты (запускают реальные бинарники)
```

### Точки входа бинарников

| Файл | Что делает |
|------|-----------|
| `app/victoria-logs/main.go` | Single-node: инициализирует vlstorage, vlselect, vlinsert, запускает HTTP на `:9428` |
| `app/vlagent/main.go` | Агент: filecollector + kubernetescollector → remotewrite → vlinsert |
| `app/vlogscli/main.go` | CLI: читает LogsQL из stdin, шлёт к `/select/logsql/query`, форматирует вывод |
| `app/vlogsgenerator/main.go` | Генератор: POST JSON-строк к настраиваемому адресу |

**Навигация:** чтобы понять любой HTTP-эндпоинт, начните с `RequestHandler` в соответствующем `app/vl*/main.go` или `app/vl*/*.go`.

---

## 2. HTTP-слой

### Запись (`app/vlinsert/`)

**Точка входа:** `app/vlinsert/main.go` → `RequestHandler()`

Роутинг по пути URL:

```
/insert/jsonline              → jsonline/
/insert/elasticsearch/...     → elasticsearch/
/insert/loki/...              → loki/
/insert/opentelemetry/...     → opentelemetry/
/insert/datadog/...           → datadog/
/insert/splunk/...            → splunk/
/insert/journald/...          → journald/
/insert/native                → nativeinsert/
/api/v2/logs                  → datadog/ (нативный путь Datadog)
/services/collector/...       → splunk/ (нативный путь Splunk)
/internal/insert              → internalinsert/ (кластерный путь vlstorage→vlinsert)
```

Каждый субпакет парсит свой формат и конвертирует логи в `logstorage.LogRows`, затем вызывает `vlstorage.Storage.MustAddRows()`.

Общая утилита для всех протоколов — `app/vlinsert/insertutil/`:
- `common_params.go` — разбор `_time_field`, `_msg_field`, `_stream_fields` из HTTP-параметров
- `line_reader.go` — построчный ридер с буфером

### Запросы (`app/vlselect/`)

**Точка входа:** `app/vlselect/main.go` → `RequestHandler()` → `selectHandler()` → `processSelectRequest()`

Ключевые маршруты:

```
/select/logsql/query           → logsql.ProcessQueryRequest()
/select/logsql/stats_query     → logsql.ProcessStatsQueryRequest()
/select/logsql/stats_query_range → logsql.ProcessStatsQueryRangeRequest()
/select/logsql/tail            → logsql.ProcessLiveTailRequest()
/select/logsql/hits            → logsql.ProcessHitsRequest()
/select/logsql/field_names     → logsql.ProcessFieldNamesRequest()
/select/logsql/field_values    → logsql.ProcessFieldValuesRequest()
/select/logsql/streams         → logsql.ProcessStreamsRequest()
/select/vmui/                  → embedded VMUI (go:embed)
```

**Ограничение параллелизма:** в `vlselect/main.go` — канальный семафор `concurrencyLimitCh` (по умолчанию `2 * кол-во CPU`, не более 16). Live-tail запросы идут в обход семафора.

Реализация хендлеров: `app/vlselect/logsql/*.go` — каждый файл соответствует одному эндпоинту.

### Конфигурация хранилища (`app/vlstorage/`)

**Точка входа:** `app/vlstorage/main.go` → `Init()` → `MustOpenStorage()`

Здесь живут все CLI-флаги, которые влияют на хранилище: `-retentionPeriod`, `-storageDataPath`, `-storageNode`, `-inmemoryDataFlushInterval` и т.д.

---

## 3. Физическая структура хранилища

```
victoria-logs-data/                  ← storageDataPath
  partitions/
    20250101/                        ← одна папка на каждый день (YYYYMMDD)
      indexdb/                       ← индекс стримов (stream → streamID)
      datadb/                        ← блоки с данными
        parts.json                   ← список активных частей
        <uuid>/                      ← одна "часть" (part)
          metadata.json
          column_names.bin           ← словарь имён колонок
          column_idxs.bin            ← индексы колонок → имена
          metaindex.bin              ← мета-индекс (список index-блоков)
          index.bin                  ← заголовки блоков (blockHeader)
          columns_header_index.bin   ← индекс заголовков колонок
          columns_header.bin         ← заголовки колонок (тип кодирования, dict, const)
          timestamps.bin             ← сжатые временны́е метки
          message_values.bin         ← значения поля _msg
          message_bloom.bin          ← bloom-фильтр для _msg
          values.bin                 ← значения остальных колонок (0..127 шардов)
          bloom.bin                  ← bloom-фильтры остальных колонок (шарды)
      snapshots/                     ← снапшоты партиции
    20250102/
    ...
  delete_tasks.json                  ← персистентные задачи удаления
```

**Ключевые константы** (`lib/logstorage/consts.go`):
- Максимальный размер блока: **2 МБ** несжатых данных
- Максимум строк в блоке: **8 млн**
- Максимум колонок в блоке: **2 000**
- Максимальный размер const-значения: **256 байт** (иначе → в values.bin)
- Bloom-фильтр: **16 бит/токен**, **6 хеш-функций** (xxhash с разными seed)
- Последняя версия формата частей: **3**

---

## 4. Движок хранилища

Иерархия абстракций, сверху вниз:

```
Storage                   (lib/logstorage/storage.go)
  └─ []partitionWrapper   (одна на каждый день)
       └─ partition        (lib/logstorage/datadb.go — внутри partition struct)
            ├─ indexdb     (индекс стримов)
            └─ datadb      (lib/logstorage/datadb.go)
                 ├─ []inmemoryPart  (принимают свежие записи)
                 ├─ []smallPart     (сброшены с памяти, < maxInmemoryPartSize)
                 └─ []bigPart       (после merge, ≤ 1 ТБ)
                      └─ part
                           └─ []blockHeader  (в index.bin)
                                └─ block     (данные в файлах)
```

### `Storage` (`storage.go`)

- Содержит `[]partitionWrapper`, отсортированных по дню (`partitions[0]` — самая старая).
- `ptwHot` — горячая партиция, куда идёт большинство записей (оптимизация для fast path).
- Два кеша: `streamIDCache` и `filterStreamCache` — оба используют двухпоколенческую схему (`cache.go`).
- Фоновые горутины: `runRetentionWatcher`, `runMaxDiskSpaceUsageWatcher`, `runDeleteTasksWatcher`, `runSnapshotsMaxAgeWatcher`.

### `datadb` (`datadb.go`)

Реализует LSM-подобную структуру с тремя уровнями частей:
- **inmemoryParts** — буфер в памяти. Данные пишутся сюда сначала. Периодически (`flushInterval`, по умолчанию 5 сек.) сбрасываются в `smallParts`.
- **smallParts** — файлы на диске. Когда их накапливается `defaultPartsToMerge` (15), запускается merge в `bigPart`.
- **bigParts** — крупные части (до 1 ТБ). Тоже периодически мержатся между собой.

Каждая часть защищена `refCount` (через `partWrapper`). Удаление — только после того, как refCount обнулится после merge.

### `block` (`block.go`) и `blockHeader` (`block_header.go`)

`block` — центральная структура при записи:
```go
type block struct {
    timestamps   []int64   // временны́е метки в наносекундах
    columns      []column  // колонки с переменными значениями
    constColumns []Field   // колонки, где все строки имеют одно значение
}
```

`constColumns` — оптимизация: если все строки в блоке имеют одно и то же значение для колонки (например, `host="web-01"`), оно хранится один раз в `columns_header.bin`, а не повторяется для каждой строки.

`blockHeader` (в `index.bin`) — это "оглавление" блока: streamID, диапазон временных меток, смещения в файлах, bloom-фильтр для быстрого пропуска блоков при поиске.

---

## 5. Путь записи (ingestion path)

```
HTTP handler (e.g., loki.RequestHandler)
  ↓
insertutil.GetCommonParams() — разбор _time_field, _msg_field, _stream_fields
  ↓
logstorage.GetLogRows() — аллокация LogRows из пула
  ↓
LogRows.MustAdd(tenantID, timestamp, fields, streamFields)
  → вычисление streamID = xxhash128(sorted stream labels)
  → streamIDCache.Get() — проверка, не новый ли стрим (чтобы не писать в indexdb лишний раз)
  ↓
vlstorage.Storage.MustAddRows(lr)
  ↓
  Fast path: ptwHot.pt.mustAddRows(lr)          ← все строки в одном дне → в горячую партицию
  Slow path: разбивка по days → getPartitionForWriting(day) → mustAddRows
  ↓
partition.mustAddRows(lr)
  → Запись в datadb.rb (rowsBuffer — кольцевой буфер в памяти)
  → Когда rb заполнился: конвертация в inmemoryPart
  ↓
Фоновая горутина в datadb:
  → inmemoryPart → flush → smallPart (по истечении flushInterval)
  → appendPartsToMerge() → merge smallParts → bigPart
```

**Ключевые файлы для записи:**
- `lib/logstorage/rows.go` — тип `LogRows`, `MustAdd()`
- `lib/logstorage/storage.go` — `MustAddRows()`
- `lib/logstorage/datadb.go` — `mustAddRows()`, логика merge
- `lib/logstorage/block_stream_writer.go` — запись блока в файлы
- `lib/logstorage/encoding.go` — сжатие временных меток (delta-of-delta + zstd)

---

## 6. Путь чтения (query path)

### Обзор

```
HTTP handler → logsql.ProcessQueryRequest()
  ↓
ParseQuery(queryStr) → *Query{filter, []pipe}
  ↓
vlstorage.Storage.RunQuery(qctx, writeBlock)
  ↓
initSubqueries() — разворачивание in(subquery), join, union, stream_context
  ↓
getSearchOptions() — извлечение time range, stream filter, needed fields
  ↓
runPipes() — создание цепочки pipeProcessor (справа налево)
  ↓
searchParallel() — параллельный поиск по партициям и блокам
  ↓
blockSearch.search() — bitmap-фильтрация строк в блоке
  ↓
writeBlock(workerID, &bs.br) → pipe[0].writeBlock() → pipe[1].writeBlock() → ...
  ↓
pp.flush() для каждого пайпа (по завершении всего поиска)
  ↓
Финальный writeBlock → JSON-сериализация → HTTP response
```

### Параллельный поиск (`storage_search.go:searchParallel`)

1. Запускается `workersCount` воркеров (горутины), которые читают из `workCh chan *blockSearchWorkBatch`.
2. Параллельно по всем партициям, попавшим в time range, запускается `partition.search()` — он итерирует по `blockHeader`-ам в index.bin и отправляет батчи `blockSearchWork` в `workCh`.
3. Каждый воркер вызывает `blockSearch.search(bsw, bm)`:
   - Инициализирует bitmap размером `bh.rowsCount` (все биты = 1).
   - Вызывает `filter.applyToBlockSearch(bs, bm)` — фильтр сбрасывает биты для несовпадающих строк.
   - Если bitmap не пуст — вызывает `writeBlock(workerID, &bs.br)`.

### Цепочка пайпов (`storage_search.go:runPipes`)

```go
// Пайпы строятся справа налево (от последнего к первому).
// Каждый пайп получает ppNext и возвращает свой pipeProcessor.
for i := len(pipes) - 1; i >= 0; i-- {
    pp = pipes[i].newPipeProcessor(concurrency, stopCh, cancel, pp)
}
// Поиск пишет в pp[0].writeBlock(), который передаёт данные в pp[1] и т.д.
search(stopCh, pp.writeBlock)
// После завершения поиска — flush по порядку
for _, pp := range pps {
    pp.flush()
}
```

Это позволяет пайпам, которым нужно накапливать данные (sort, uniq, stats), работать корректно: `writeBlock` накапливает данные в per-worker состоянии, `flush` агрегирует и передаёт дальше.

### `blockResult` (`block_result.go`)

Главная структура, передаваемая между пайпами. Ключевые поля:
- `rowsLen int` — кол-во строк в батче
- `csBuf []blockResultColumn` — набор колонок (ленивые: значения читаются из файлов по запросу)
- `timestampsBuf []int64` — временны́е метки строк
- `a arena` — arena-аллокатор для строковых данных (избегает heap-аллокаций)

Колонки ленивые: `getValues(br)` декодирует данные из файла при первом обращении и кешируется в `valuesCache` внутри `blockSearch`.

---

## 7. LogsQL: парсер, фильтры, пайпы

### Парсер (`lib/logstorage/parser.go`)

Написан вручную (рекурсивный спуск). Лексер — `lexer` struct с методом `nextToken()`.

**Точки входа парсера:**
```go
ParseQuery(s string) (*Query, error)           // для большинства запросов
ParseQueryAtTimestamp(s, timestamp)             // когда нужен конкретный timestamp для _time:duration
ParseFilter(s string) (*Filter, error)          // только для фильтра (без пайпов)
```

**Структура `Query`:**
```go
type Query struct {
    opts  queryOptions   // concurrency, parallel_readers, time_offset, global_filter, ...
    f     filter         // фильтр (часть до первого |)
    pipes []pipe         // цепочка пайпов (после |)
}
```

После парсинга вызывается `q.optimize()` — пакет оптимизаций:
- Слияние вложенных AND/OR
- Перемещение `| filter` в основной фильтр (merge)
- Оптимизация `sort | limit` → сохранение только N лучших
- Оптимизация `| offset N | limit M` → пропуск правильного числа строк

### Фильтры (`lib/logstorage/filter.go`, `filter_*.go`)

Интерфейс `filter`:
```go
type filter interface {
    String() string
    updateNeededFields(pf *prefixfilter.Filter)
    matchRow(fields []Field) bool              // построчная проверка (используется редко)
    applyToBlockSearch(bs *blockSearch, bm *bitmap)  // блочная проверка (основной путь)
    applyToBlockResult(br *blockResult, bm *bitmap)  // проверка внутри пайпов
}
```

**Как работает `applyToBlockSearch`:** метод получает bitmap (все биты = 1 при входе) и сбрасывает биты для строк, не совпадающих с фильтром. Это позволяет AND-фильтрам последовательно сужать результат без дополнительных аллокаций.

Оптимизация на уровне блока: перед тем как читать значения из файла, `applyToBlockSearch` проверяет bloom-фильтр блока. Если ни один из токенов запроса не присутствует в bloom-фильтре — весь блок пропускается без чтения данных.

**Таблица типов фильтров:**

| Файл | Фильтр в LogsQL | Описание |
|------|-----------------|----------|
| `filter_exact.go` | `field:"value"` | Точное совпадение |
| `filter_phrase.go` | `"phrase"` | Фраза (слово или набор слов) |
| `filter_prefix.go` | `prefix*` | Префикс |
| `filter_regexp.go` | `field:~"regex"` | Регулярное выражение |
| `filter_range.go` | `field:>=N` | Диапазон числовых значений |
| `filter_time.go` | `_time:...` | Фильтр по времени |
| `stream_filter.go` | `{job="app"}` | Фильтр по стримам |
| `filter_and.go` | `a AND b` | Логическое И |
| `filter_or.go` | `a OR b` | Логическое ИЛИ |
| `filter_not.go` | `NOT a` | Логическое НЕ |
| `filter_in.go` | `field:in(...)` | Фильтр по списку / подзапросу |

### Пайпы (`lib/logstorage/pipe.go`, `pipe_*.go`)

Интерфейс `pipe`:
```go
type pipe interface {
    String() string
    updateNeededFields(pf *prefixfilter.Filter)     // какие колонки нужны
    newPipeProcessor(concurrency int, stopCh <-chan struct{}, cancel func(), ppNext pipeProcessor) pipeProcessor
    splitToRemoteAndLocal(timestamp int64) (pipe, []pipe) // для кластера
    canLiveTail() bool
    // ...
}
```

Интерфейс `pipeProcessor`:
```go
type pipeProcessor interface {
    writeBlock(workerID uint, br *blockResult)  // вызывается конкурентно из N воркеров
    flush() error                               // вызывается после завершения поиска
}
```

Паттерн per-worker state: пайпы используют `atomicutil.Slice[T]` — slice, который индексируется `workerID`. Это позволяет избежать mutex при параллельной записи из разных воркеров. Агрегация происходит в `flush()`.

**Полный список пайпов** регистрируется в `initPipeParsers()` в `pipe.go` (~55 пайпов). Ключевые:

| Пайп | Файл | Описание |
|------|------|----------|
| `filter` / `where` | `pipe_filter.go` | Дополнительная фильтрация |
| `stats` | `pipe_stats.go` | Агрегации (count, sum, avg, ...) |
| `sort` | `pipe_sort.go` | Сортировка (heap для top-N) |
| `limit` / `head` | `pipe_limit.go` | Ограничение числа строк |
| `uniq` | `pipe_uniq.go` | Дедупликация |
| `fields` / `keep` | `pipe_fields.go` | Выбор колонок |
| `delete` / `drop` | `pipe_delete.go` | Удаление колонок |
| `format` | `pipe_format.go` | Форматирование строк |
| `extract` | `pipe_extract.go` | Извлечение полей из строки |
| `unpack_json` | `pipe_unpack_json.go` | Разбор JSON-поля |
| `join` | `pipe_join.go` | JOIN с подзапросом |
| `union` | `pipe_union.go` | UNION нескольких запросов |
| `stream_context` | `pipe_stream_context.go` | Контекст соседних строк стрима |

---

## 8. Ключевые алгоритмы и подходы

### Bloom-фильтр для быстрого пропуска блоков

**Файлы:** `bloomfilter.go`, `tokenizer.go`

При записи блока каждое строковое значение каждой колонки токенизируется: текст разбивается на слова (границы — пробелы и не-алфавитные символы). Из токенов строится bloom-фильтр (16 бит/токен, 6 хешей xxhash64).

При поиске: `applyToBlockSearch` для текстовых фильтров (`filter_phrase`, `filter_prefix`, `filter_exact`) сначала проверяет bloom-фильтр блока. Если bloom говорит "нет" — блок пропускается целиком без чтения данных колонок (I/O saved).

Bloom-фильтр хранится в `message_bloom.bin` (для `_msg`) и в `bloom.bin` (по одному шарду на диапазон колонок).

### Колоночное кодирование (`values_encoder.go`)

При записи части движок автоматически определяет оптимальный тип кодирования для каждой колонки блока:

| Тип | Условие | Хранение |
|-----|---------|----------|
| `valueTypeDict` | ≤ 8 уникальных значений, суммарно ≤ 256 байт | индексы в словарь (1 байт/строка) |
| `valueTypeUint8/16/32/64` | все значения — неотрицательные целые | фиксированный размер |
| `valueTypeInt64` | все значения — знаковые целые | 8 байт |
| `valueTypeFloat64` | все значения — числа с плавающей точкой | 8 байт |
| `valueTypeIPv4` | все значения — IPv4-адреса | 4 байта |
| `valueTypeTimestampISO8601` | все значения — ISO8601 timestamps | 8 байт (nanoseconds) |
| `valueTypeString` | всё остальное | строки as-is + zstd |

Если все строки блока имеют одно значение — колонка помечается как `constColumn` и хранится только один раз в `columns_header.bin`.

Определение типа происходит при создании `inmemoryPart` — сканируется весь набор значений колонки в блоке.

### Потоковая обработка без копирования

`blockResult` содержит `arena` — монотонный bump-аллокатор. Все строки внутри `blockResult` ссылаются на эту арену — нет heap-аллокаций на каждую строку. При `br.reset()` арена сбрасывается целиком.

Аналогично в фильтрах: `applyToBlockSearch` работает с данными прямо из mmap/файла через unsafe-ссылки (zero-copy).

### Двухпоколенческий кеш стримов (`cache.go`)

```go
type cache struct {
    curr atomic.Pointer[sync.Map]
    prev atomic.Pointer[sync.Map]
}
```

Каждые ~3 минуты `curr` становится `prev`, а `curr` заменяется пустой `sync.Map`. При Get: ищем в `curr`, если нет — ищем в `prev` и при нахождении "прогреваем" обратно в `curr`. Это автоматически вытесняет редко используемые стримы без LRU-overhead.

`streamIDCache` используется при записи: перед регистрацией нового стрима в `indexdb` проверяется кеш — если стрим уже видели, пропускаем дорогую запись в индекс.

`filterStreamCache` используется при поиске по `{label="value"}` — кешируются списки streamID для данного фильтра.

### Stream Index (`indexdb`)

Stream — это набор меток (`{job="app", host="web-01"}`). При первом появлении нового набора меток:
1. Вычисляется `streamID` — xxhash128 от канонически отсортированных меток.
2. Маппинг `(streamID → метки)` записывается в `indexdb`.
3. `streamID` сохраняется в каждой строке и в каждом `blockHeader`.

При запросе `_stream:{job="app"}` движок сначала идёт в `indexdb`, находит все `streamID` для данного фильтра (используя `filterStreamCache`), и затем ищет только блоки с этими `streamID` — минуя все остальные блоки.

### Оптимизация last-N (`app/vlstorage/lastnoptimization.go`)

Для запросов вида `* | sort by (_time) desc | limit 100` движок умеет читать блоки партиций в обратном хронологическом порядке и останавливаться как только набрано N строк — без чтения всего датасета.

---

## 9. Кластерный режим

В кластерном режиме роли разделяются флагами:

```
vlinsert-узел: -select.disable
vlselect-узел: -insert.disable
vlstorage-узел: -insert.disable -select.disable
```

Связующее звено — флаг `-storageNode` на insert/select-узлах. Когда он задан, `app/vlstorage/main.go` инициализирует **не локальное хранилище**, а сеть клиентов к storage-узлам (`netinsert/`, `netselect/`).

```
vlinsert-узел
  ↓ POST /internal/insert (protobuf)
vlstorage-узел (накапливает, хранит, отвечает на /internal/select/*)

vlselect-узел
  ↓ POST /internal/select/* (protobuf)
vlstorage-узел (поиск + агрегация на стороне storage)
  ↓ результат
vlselect-узел (финальная агрегация если нужна)
```

Для каждого пайпа метод `splitToRemoteAndLocal()` определяет, что можно делегировать на storage-узел, а что надо доделать локально. Например, `stats` пайп может частично агрегировать на storage, а финальное слияние делается на select.

---

## 10. Паттерны управления памятью

### `sync.Pool` везде

Практически все крупные структуры (`blockSearch`, `blockSearchWorkBatch`, `bitmap`, `bloomFilter`, `inmemoryPart`, `LogRows`, и т.д.) хранятся в `sync.Pool`. Типичный паттерн:

```go
func getBlockSearch() *blockSearch {
    v := blockSearchPool.Get()
    if v == nil { return &blockSearch{} }
    return v.(*blockSearch)
}
func putBlockSearch(bs *blockSearch) {
    bs.reset()
    blockSearchPool.Put(bs)
}
```

### `atomicutil.Slice[T]`

Используется для per-worker состояния в пайпах. `Get(workerID)` возвращает указатель на элемент для данного воркера без блокировок. Slice растёт автоматически при появлении новых `workerID`.

```go
var shards atomicutil.Slice[myShard]
// в writeBlock:
shard := shards.Get(workerID)  // lock-free
shard.data = append(shard.data, ...)
```

### `arena` (`arena.go`)

Монотонный аллокатор для строк внутри `blockResult`. При `br.reset()` вся память возвращается разом — без вызова GC на каждую строку.

### `chunkedAllocator` (`chunked_allocator.go`)

Используется в пайпах, которым нужно хранить строки между вызовами `writeBlock` (например, `pipe_uniq`, `pipe_sort`). Аллоцирует большие чанки и нарезает строки из них — снижает давление на GC при большом числе уникальных строк.

---

## 11. Что хранится в памяти и как данные попадают на диск

### Уровни буферизации

Путь записи от HTTP-запроса до диска проходит три уровня в памяти:

```
HTTP handler
  │
  ▼
rowsBuffer.shard.lr   ← уровень 1: сырые строки (pre-buffer)
  │  flush: ~1 сек или по размеру
  ▼
inmemoryPart          ← уровень 2: полностью индексированная часть в RAM
  │  flush: flushInterval (5 сек по умолчанию) или при shutdown
  ▼
smallPart / bigPart   ← уровень 3: файлы на диске
```

---

### Уровень 1: `rowsBuffer` — pre-buffer сырых строк

**Файл:** `datadb.go`, тип `rowsBuffer`

`datadb.rb` — это шардированный буфер, куда попадают все вызовы `mustAddRows`. Шардов ровно столько, сколько CPU (`cgroup.AvailableCPUs()`). Распределение — round-robin по счётчику `rb.nextIdx`.

Каждый шард (`rowsBufferShard`) содержит:
- `mu sync.Mutex` — защита шарда
- `lr *logRows` — текущий буфер сырых строк (поля `streamIDs`, `timestamps`, `rows [][]Field`)
- `flushTimer *time.Timer` — таймер на 1 секунду, запускаемый при первой записи в шард

**Триггеры сброса шарда:**
1. `lr.needFlush()` — размер данных в `lr` достиг лимита (вызывается синхронно при каждой записи)
2. `time.AfterFunc(1 second)` — таймер сработал (асинхронный flush)

При сбросе вызывается `mustFlushLogRows(lr)`:
1. Сортировка строк по `(streamID, timestamp)` — `sort.Sort(lr)`
2. Сортировка полей внутри каждой строки
3. Создание `inmemoryPart` из отсортированных данных
4. Добавление в `ddb.inmemoryParts`

**Важно:** данные в `rowsBuffer` ещё не поисковые — они не видны при запросах LogsQL до момента преобразования в `inmemoryPart`. Эту задержку отражает метрика `vl_pending_rows{type="storage"}`.

---

### Уровень 2: `inmemoryPart` — индексированная часть в памяти

**Файл:** `inmemory_part.go`, тип `inmemoryPart`

`inmemoryPart` — это полный аналог дисковой части, но хранящийся в `chunkedbuffer.Buffer` вместо файлов. Содержит те же данные, что и файлы на диске:

```go
type inmemoryPart struct {
    ph                 partHeader          // метаданные (счётчики, диапазон времён)
    columnNames        chunkedbuffer.Buffer // = column_names.bin
    columnIdxs         chunkedbuffer.Buffer // = column_idxs.bin
    metaindex          chunkedbuffer.Buffer // = metaindex.bin
    index              chunkedbuffer.Buffer // = index.bin
    columnsHeaderIndex chunkedbuffer.Buffer // = columns_header_index.bin
    columnsHeader      chunkedbuffer.Buffer // = columns_header.bin
    timestamps         chunkedbuffer.Buffer // = timestamps.bin
    messageBloomValues bloomValuesBuffer   // = message_bloom.bin + message_values.bin
    fieldBloomValues   bloomValuesBuffer   // = bloom.bin0 + values.bin0 (один шард)
}
```

`chunkedbuffer.Buffer` — это буфер с чанками фиксированного размера (не растущий слайс), чтобы не копировать данные при росте. Поиск по `inmemoryPart` работает через те же `blockStreamReader`/`blockSearch`, что и при поиске по файлам.

**Создание `inmemoryPart` из `logRows`:**
1. Группировка по `streamID`
2. Нарезка на блоки по `maxUncompressedBlockSize` (2 МБ)
3. Для каждого блока:
   - Кодирование временны́х меток (выбор лучшего из 6 типов кодирования)
   - Определение `valueType` каждой колонки (пробуем dict → uint → int → float → ipv4 → iso8601 → string)
   - Построение bloom-фильтра для каждой колонки
   - Запись в соответствующий `chunkedbuffer.Buffer`

**Ограничения на `inmemoryPart`:**
- Максимальный размер одной части: `getMaxInmemoryPartSize()` = `10% × memory.Allowed() / 20`. При `memory.Allowed()` = 8 ГБ это ≈ 40 МБ на часть.
- Максимальное число inmemory-частей: `maxInmemoryPartsPerPartition` = 20
- Дедлайн сброса на диск: `time.Now() + flushInterval` (флаг `-inmemoryDataFlushInterval`, дефолт 5 сек)

**Инварианты на время жизни:**
- `partWrapper.refCount` — атомарный счётчик ссылок. Увеличивается при старте поиска, уменьшается при его завершении.
- `partWrapper.isInMerge` — защита от одновременного выбора части двумя merge-горутинами (выставляется под `partsLock`).
- `partWrapper.mustDrop` — флаг «удалить при обнулении refCount». Выставляется после включения merge-результата в список активных частей.

---

### Уровень 3: файловые части (smallPart / bigPart)

Определение типа назначения результата merge:

```
dstSize > getMaxSmallPartSize()              → partBig
isFinal || dstSize > getMaxInmemoryPartSize() → partSmall  
все источники — inmemory && !isFinal          → partInmemory
```

`getMaxSmallPartSize()` = `min(memory.Remaining() / 15, available_disk_space)`. Это значит: small parts должны помещаться в page cache (примерно).

**Отличие bigPart от smallPart:**
- `bigPart` записывается с `nocache=true` — `O_DIRECT` флаг, данные обходят page cache при записи. Это позволяет не вытеснять hot-данные small-частей из кеша ядра.
- `smallPart` записывается обычно — попадает в page cache и доступен без I/O при следующем чтении.

---

### Flush inmemoryParts → диск

**Файл:** `datadb.go:mustFlushInmemoryPartsToFiles()`

Фоновая горутина `inmemoryPartsFlusher` тикает каждые `flushInterval` (5 сек) и вызывает `mustFlushInmemoryPartsToFiles(false)`. Она смотрит на `pw.flushDeadline` каждой inmemory-части и сбрасывает на диск те, чей дедлайн прошёл.

**Быстрый путь** (одна часть, без merge): `mp.MustStoreToDisk(path)` — параллельная запись всех `chunkedbuffer.Buffer`-ов в файлы через `filestream.ParallelStreamWriter`.

**Медленный путь** (несколько частей): запускается K-way merge через `mustMergeBlockStreams()`.

**При graceful shutdown** (SIGTERM, `mustCloseDatadb()`):
1. `rb.flush()` — сбросить все шарды `rowsBuffer` в `inmemoryPart`
2. `close(stopCh)` → все merge-горутины останавливаются
3. `ddb.wg.Wait()` — дождаться завершения текущих merge
4. `mustFlushInmemoryPartsToFiles(true)` — сбросить ВСЕ inmemory-части, игнорируя `flushDeadline` и `stopCh`

---

### Merge: алгоритм выбора частей

**Файл:** `datadb.go:appendPartsToMerge()`

Merge не запускается просто «когда частей стало много». Реализован жадный алгоритм минимизации write amplification:

1. **Фильтрация:** отбросить части > `maxOutBytes / minMergeMultiplier` (1.7) — они слишком большие, их merge только навредит write amplification.

2. **Сортировка** оставшихся по размеру (по возрастанию), при равном — по убыванию временно́й метки. Это улучшает локальность данных в результирующей части.

3. **Exhaustive search** по всем contiguous subsets размером от `ceil(defaultPartsToMerge/2)` до `defaultPartsToMerge` (= от 8 до 15):
   - Пропускаем если `parts[0].size * count < parts[last].size` — слишком несбалансированно
   - Пропускаем если общий выход > `maxOutBytes`
   - Считаем «коэффициент слияния» `m = outSize / maxInputPartSize`
   - Запоминаем subset с максимальным `m`

4. **Порог:** если лучшее `m < minMergeMultiplier` (1.7) — merge отменяется. Нет смысла мержить, если не достигается хотя бы 1.7× роста относительно самой большой входной части.

**Параллелизм merge:** три независимых канальных семафора (каждый с capacity = `AvailableCPUs()`):
- `inmemoryPartsConcurrencyCh` — для inmemory→inmemory и inmemory→disk
- `smallPartsConcurrencyCh` — для small→small и small→big
- `bigPartsConcurrencyCh` — для big→big

---

### Merge: исполнение

**Файл:** `datadb.go:mustMergePartsInternal()`

```
1. Проверить/зарезервировать место на диске (reservedDiskSpace atomic)
2. Открыть blockStreamReader для каждой исходной части
3. Открыть blockStreamWriter для результата:
   - inmemory: инициализируется из нового inmemoryPart
   - файл: path = <datadb.path>/<mergeIdx:016X>/
4. mustMergeBlockStreams() — потоковый K-way merge:
   - Читает blockHeader из всех источников
   - Объединяет в порядке (streamID, minTimestamp)
   - Применяет dropFilter если задан (для удаления логов)
   - Пишет результат через blockStreamWriter
5. Записать metadata.json, sync директории
6. swapSrcWithDstParts() — под partsLock:
   a. Добавить новую часть в нужный список
   b. Записать обновлённый parts.json (атомарно — через temp file + rename)
   c. Убрать старые части из списка
   d. Выставить pw.mustDrop = true на старых частях
   e. decRef() — если refCount == 0, удалить директорию
```

**Атомарность `parts.json`:** `fs.MustWriteAtomic` пишет в temp-файл, затем делает rename. Это гарантирует: при сбое видим либо старый, либо новый список — никогда половинчатый.

---

### Что теряется при сбое

| Слой | Что содержит | Теряется при сбое |
|------|-------------|-------------------|
| `rowsBuffer.shard.lr` | Последние ≤1 сек данных | **Да** |
| `inmemoryPart` (до flush) | Данные за ≤5 сек | **Да** |
| Файл в процессе merge | Incomplete directory не в parts.json | Нет (очищается при запуске) |
| `smallPart` / `bigPart` | Данные с подтверждённым parts.json | **Нет** |

При открытии `datadb` вызывается `mustRemoveUnusedDirs()` — он удаляет все поддиректории, которых нет в `parts.json`. Так убираются «осиротевшие» директории от прерванных merge.

Уменьшить потенциальную потерю данных можно флагом `-inmemoryDataFlushInterval` (например, `1s`), но это увеличивает нагрузку на диск.

---

### Схема жизненного цикла части

```
[ingestion goroutine]
  MustAddRows(lr)
    └─ rb.shard[cpu].lr ← append rows
         │
         ├── [timer 1s / needFlush()] mustFlushLogRows(lr)
         │     └─ sort + encode → inmemoryPart (in RAM, in ddb.inmemoryParts)
         │
         └── [timer 5s / shutdown] mustFlushInmemoryPartsToFiles()
               ├── fast path (1 part): MustStoreToDisk() → smallPart
               └── slow path (N parts): mustMergeBlockStreams() → smallPart/bigPart

[background merger goroutines × AvailableCPUs each type]
  inmemoryPartsMerger:
    └─ inmemory + inmemory → inmemory (если результат < maxInmemoryPartSize)
    └─ inmemory + inmemory → smallPart (если результат больше или isFinal)
  smallPartsMerger:
    └─ small × [8..15] → smallPart / bigPart
  bigPartsMerger:
    └─ big × [8..15] → bigPart

[при достижении retention]
  storage.runRetentionWatcher()
    └─ удаляет целые партиции (папки по дням) у которых MaxTimestamp < now-retention

[при запросе на удаление]
  deleteRows(dropFilter)
    └─ находит части с совпадающими строками
    └─ mustMergePartsInternal(..., dropFilter) — merge с выбрасыванием совпадений
```

---

## 12. Бинарный формат файлов в Part

Каждая «часть» (part) на диске — это директория `<uuid>/` с набором бинарных файлов. Исходный код: `lib/logstorage/filenames.go`, `block_header.go`, `column_names.go`, `bloomfilter.go`, `encoding.go`.

### Обзор файлов

```
<uuid>/
  metadata.json              ← статистика части (JSON)
  column_names.bin           ← словарь имён колонок (zstd)
  column_idxs.bin            ← маппинг колонка → шард values/bloom (raw)
  metaindex.bin              ← верхний уровень индекса (zstd)
  index.bin                  ← заголовки блоков (zstd-блоки)
  columns_header_index.bin   ← быстрый поиск колонки в block (raw)
  columns_header.bin         ← метаданные колонок блока (raw)
  timestamps.bin             ← временны́е метки (delta/delta-of-delta/const)
  message_values.bin         ← значения поля _msg
  message_bloom.bin          ← bloom-фильтр для _msg
  values.bin0 .. values.bin127   ← значения остальных колонок (0..127 шардов)
  bloom.bin0  .. bloom.bin127    ← bloom-фильтры остальных колонок (шарды)
```

Максимальное число шардов для values/bloom: **128** (`bloomValuesMaxShardsCount`). Реальное число шардов в конкретной части хранится в `metadata.json` поле `BloomValuesShardsCount`.

---

### `metadata.json`

Сериализованный `partHeader`. Читается при открытии части:

```json
{
  "FormatVersion": 3,
  "UncompressedSizeBytes": 1048576,
  "RowsCount": 50000,
  "BlocksCount": 12,
  "MinTimestamp": 1720000000000000000,
  "MaxTimestamp": 1720003600000000000,
  "BloomValuesShardsCount": 4
}
```

`FormatVersion` = 3 — текущая последняя версия. При изменении числа шардов версия обновляется.

---

### `column_names.bin`

Глобальный словарь имён колонок для данной части. Один файл на всю часть (не на блок).

**Формат** (zstd-сжатый блок):
```
[count: varint]
  [name_0_len: varint][name_0: bytes]
  [name_1_len: varint][name_1: bytes]
  ...
```

ID колонки = порядковый индекс (0, 1, 2, ...). Используется везде, где нужно ссылаться на имя колонки — в `column_idxs.bin`, `columns_header_index.bin` и в `columnsHeader`. Читается один раз при открытии части и кешируется в `p.columnNames []string`.

---

### `column_idxs.bin`

Маппинг ID колонки → номер шарда (в `values.binN` / `bloom.binN`).

**Формат** (raw binary, без сжатия):
```
[count: varint]
  [columnID: varint][shardIdx: varint]
  [columnID: varint][shardIdx: varint]
  ...
```

Шард вычисляется при записи как `nextColumnIdx % 128` — то есть колонки распределяются по шардам round-robin в порядке первого появления. Несколько колонок могут попасть в один шард.

---

### `metaindex.bin`

Верхний уровень двухуровневого индекса. **Один zstd-блок** на весь файл. После распаковки — плоский массив `indexBlockHeader`:

```
[streamID: 24 bytes][minTimestamp: 8 bytes BE][maxTimestamp: 8 bytes BE]
[indexBlockOffset: 8 bytes BE][indexBlockSize: 8 bytes BE]
= 56 bytes per entry
```

`streamID` = 16 байт xxhash128 от набора stream-лейблов + 8 байт `TenantID{AccountID, ProjectID}`.

Записи **отсортированы по `streamID`**. Это позволяет бинарным поиском найти диапазон `indexBlockHeader`-ов, которые могут содержать нужный стрим, и не читать весь `index.bin`.

---

### `index.bin`

Содержит `blockHeader`-ы, сгруппированные в «индексные блоки». Каждый индексный блок — отдельный zstd-сжатый фрагмент файла размером до **128 КБ** распакованных данных (`maxUncompressedIndexBlockSize`). Смещение и размер каждого фрагмента хранятся в соответствующем `indexBlockHeader` из `metaindex.bin`.

**Структура одного `blockHeader`** (в распакованном виде):
```
streamID                         (24 bytes)
uncompressedSizeBytes            (varint)
rowsCount                        (varint)
timestampsHeader:
  blockOffset                    (8 bytes BE)
  blockSize                      (8 bytes BE)
  minTimestamp                   (8 bytes BE, nanoseconds)
  maxTimestamp                   (8 bytes BE, nanoseconds)
  marshalType                    (1 byte)
columnsHeaderIndexOffset         (varint)
columnsHeaderIndexSize           (varint)
columnsHeaderOffset              (varint)
columnsHeaderSize                (varint)
```

Все `blockHeader`-ы в индексном блоке **отсортированы по (streamID, minTimestamp)**. При поиске движок итерирует по ним и пропускает те, чей `minTimestamp..maxTimestamp` не пересекается с запрошенным диапазоном.

---

### `columns_header_index.bin`

Позволяет найти метаданные конкретной колонки внутри `columns_header.bin` по её ID без десериализации всего `columnsHeader`. Хранится как поток байт; смещение и размер для каждого блока берутся из `blockHeader.columnsHeaderIndexOffset/Size`.

**Формат** (raw, не сжат):
```
[count of columnHeadersRefs: varint]
  [columnNameID: varint][offset_in_columnsHeader: varint]
  ...
[count of constColumnsRefs: varint]
  [columnNameID: varint][offset_in_columnsHeader: varint]
  ...
```

Разделение на `columnHeadersRefs` (переменные колонки) и `constColumnsRefs` (константные колонки) позволяет при чтении сразу знать тип, не читая сами данные.

---

### `columns_header.bin`

Метаданные колонок для каждого блока. Смещение и размер для каждого блока берутся из `blockHeader.columnsHeaderOffset/Size`. **Не сжат** — данные небольшие, а доступ происходит по случайному смещению.

**Формат одного `columnsHeader`**:
```
[count of variable columnHeaders: varint]
  for each columnHeader:
    [valueType: 1 byte]
    [type-specific fields (см. ниже)]
[count of constColumns: varint]
  for each constColumn:
    [value_len: varint][value: bytes]
    # имя колонки — через constColumnsRefs[i].columnNameID → columnNames[id]
```

**Поля `columnHeader` в зависимости от `valueType`:**

| valueType | Byte | Дополнительные поля | Примечание |
|-----------|------|---------------------|------------|
| `string`  | 1    | valuesOffset(v) + valuesSize(v) + bloomOffset(v) + bloomSize(v) | строки as-is, zstd если > 128 байт |
| `dict`    | 2    | valuesDict[count(1) + ≤8 строк] + valuesOffset(v) + valuesSize(v) | без bloom, dict хранится прямо в header |
| `uint8`   | 3    | min(1) + max(1) + valuesOffset(v) + valuesSize(v) + bloomOffset(v) + bloomSize(v) | 1 байт/строка |
| `uint16`  | 4    | min(2 BE) + max(2 BE) + ... | 2 байта/строка |
| `uint32`  | 5    | min(4 BE) + max(4 BE) + ... | 4 байта/строка |
| `uint64`  | 6    | min(8 BE) + max(8 BE) + ... | 8 байт/строка |
| `int64`   | 10   | min(8 BE signed) + max(8 BE signed) + ... | 8 байт/строка |
| `float64` | 7    | min(8 BE, math.Float64bits) + max(8 BE) + ... | IEEE 754 → uint64 |
| `ipv4`    | 8    | min(4 BE) + max(4 BE) + ... | uint32 big-endian |
| `iso8601` | 9    | min(8 BE, ns) + max(8 BE, ns) + ... | int64 nanoseconds |

`(v)` = varint (1–9 байт, little-endian 7-bit encoding).

`minValue`/`maxValue` позволяют пропустить блок при числовом range-запросе (`field:>100`), не читая значения из файла.

**Константная колонка** (`constColumn`) — колонка, у которой все строки в блоке имеют одно и то же значение. Значение хранится один раз в `columns_header.bin`. Имя — через `constColumnsRefs` → `columnNames`.

---

### `timestamps.bin`

Блоки временны́х меток, хранятся последовательно. Каждый блок соответствует одному `blockHeader`, его позиция определяется `timestampsHeader.blockOffset/blockSize`.

**Тип кодирования** (`timestampsHeader.marshalType`, 1 байт):

| Тип | Значение | Описание |
|-----|----------|----------|
| `MarshalTypeConst` | 3 | Все метки одинаковые — хранится только одно значение |
| `MarshalTypeDeltaConst` | 2 | Константный шаг между метками (e.g., каждые 1ms) |
| `MarshalTypeNearestDelta2` | 5 | Delta-of-delta (Горилла-стиль), без zstd |
| `MarshalTypeZSTDNearestDelta2` | 1 | Delta-of-delta + zstd |
| `MarshalTypeNearestDelta` | 6 | Простое дельта-кодирование, без zstd |
| `MarshalTypeZSTDNearestDelta` | 4 | Дельта + zstd |

При записи движок пробует все типы и выбирает результат наименьшего размера. Временны́е метки хранятся в **наносекундах** с Unix-эпохи.

---

### `message_values.bin` и `values.bin{N}`

Блоки значений колонок. `message_values.bin` — для поля `_msg` (пустое имя колонки). `values.bin{N}` — для остальных колонок (шард N).

Каждый блок значений — это `marshalBytesBlock`:
```
[type: 1 byte]
  0x00 = plain: [len: 1 byte][data: len bytes]   # если < 128 байт
  0x01 = zstd:  [compressed_len: varint][zstd(data)]
```

Внутри данных — в зависимости от `valueType` колонки:

| valueType | Байт на строку | Формат внутри блока |
|-----------|---------------|---------------------|
| `string`  | переменный    | `marshalUint64Block(lengths)` + конкатенация строк |
| `dict`    | 1             | N байт (индекс в `valuesDict` из `columnHeader`) |
| `uint8`   | 1             | N байт as-is |
| `uint16`  | 2             | N × 2 байт (big-endian) |
| `uint32`  | 4             | N × 4 байт (big-endian) |
| `uint64`  | 8             | N × 8 байт (big-endian) |
| `int64`   | 8             | N × 8 байт (big-endian, signed) |
| `float64` | 8             | N × 8 байт (math.Float64bits, big-endian) |
| `ipv4`    | 4             | N × 4 байт (big-endian uint32) |
| `iso8601` | 8             | N × 8 байт (nanosecond int64, big-endian) |

Для `valueTypeString`, длины хранятся через `marshalUint64Block` — адаптивное кодирование uint64-массива:
```
[blockType: 1 byte]
  0 → N × uint8   1 → N × uint16   2 → N × uint32   3 → N × uint64
  4 → const uint8  5 → const uint16  6 → const uint32  7 → const uint64
[values: blockType-specific bytes]
```

Типы `const*` (4–7) экономят место когда все строки имеют одинаковую длину.

---

### `message_bloom.bin` и `bloom.bin{N}`

Bloom-фильтры для блоков. `message_bloom.bin` — для `_msg`, `bloom.bin{N}` — для колонок в шарде N.

**Формат**: плоский массив `uint64`-слов (big-endian), без заголовка. Размер и смещение берутся из `columnHeader.bloomFilterOffset/bloomFilterSize`.

**Алгоритм**:
- 16 бит на токен (`bloomFilterBitsPerItem = 16`)
- 6 хеш-функций (`bloomFilterHashesCount = 6`)
- Для каждого токена из значений:
  1. `h₀ = xxhash64(token)` — базовый хеш
  2. `h₁..h₅` = 6 последовательных хешей от 8-байтового представления `h₀` (инкрементируя значение после каждого хеша)
  3. Каждый `hᵢ % totalBits` → устанавливает один бит в массиве
- Токены для разных `valueType` разные: для `string` — слова из текста; для `uint*`/`int64`/`float64`/`ipv4`/`iso8601` — hex-представление числового значения

**Пропуск блоков при поиске**: перед чтением значений из `values.bin` движок проверяет bloom-фильтр. Если ни один из токенов запроса не встречается в фильтре — блок пропускается целиком (`I/O saved`).

---

### Схема связей между файлами

```
metaindex.bin
  [indexBlockHeader]──offset/size──► index.bin
                                        [blockHeader]
                                          timestampsHeader.blockOffset/Size──► timestamps.bin
                                          columnsHeaderIndexOffset/Size──► columns_header_index.bin
                                            [columnNameID + offset]──► columns_header.bin
                                                [columnHeader]
                                                  valuesOffset/Size──► message_values.bin
                                                                        values.bin{N}
                                                  bloomOffset/Size──► message_bloom.bin
                                                                        bloom.bin{N}
                                          columnsHeaderOffset/Size──► columns_header.bin
column_names.bin──[id→name]──► (используется везде, где встречается columnNameID)
column_idxs.bin──[name→shardN]──► (определяет N для values.binN / bloom.binN)
```

---

## Путеводитель по задачам

| Задача | С чего начать |
|--------|---------------|
| Добавить новый протокол записи | `app/vlinsert/` — создать новый субпакет по образцу `loki/` или `jsonline/`, зарегистрировать в `vlinsert/main.go:insertHandler` |
| Добавить новый фильтр LogsQL | Создать `filter_xxx.go`, реализовать интерфейс `filter`, зарегистрировать в `parser.go` (поиск по `parsePhraseFilter` для образца парсинга) |
| Добавить новый пайп | Создать `pipe_xxx.go`, реализовать интерфейсы `pipe` и `pipeProcessor`, зарегистрировать в `pipe.go:initPipeParsers` |
| Добавить новую stats-функцию | Создать `stats_xxx.go` по образцу `stats_count.go`, зарегистрировать в `pipe_stats.go` |
| Понять, как данные попадают на диск | `storage.go:MustAddRows` → `datadb.go:mustAddRows` → `block_stream_writer.go` |
| Понять, как выполняется поиск | `storage_search.go:RunQuery` → `runPipes` → `searchParallel` → `block_search.go:search` |
| Понять кодирование данных | `values_encoder.go` (типы) + `encoding.go` (дельта-кодирование timestamps) + `bloomfilter.go` |
| Отладить медленный запрос | Логи slow queries (`-search.logSlowQueryDuration`), метрики `vl_http_request_duration_seconds`, pprof через `/debug/pprof/` |
