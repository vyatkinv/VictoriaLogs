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
