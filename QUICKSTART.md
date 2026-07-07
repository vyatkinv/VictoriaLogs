# QUICKSTART.md

Практический старт с VictoriaLogs: сборка, запуск, загрузка логов и поиск по ним через LogsQL.

---

## 1. Сборка

```bash
make victoria-logs
# Бинарник окажется в bin/victoria-logs
```

Для сборки всех компонентов (включая vlagent и vlogscli):

```bash
make all
```

---

## 2. Запуск

```bash
mkdir -p /tmp/vldata

./bin/victoria-logs \
  -storageDataPath=/tmp/vldata \
  -httpListenAddr=:9428
```

Проверка что сервер живой:

```bash
curl http://localhost:9428/health
# OK
```

Сервер поднимается мгновенно. Все данные хранятся в `-storageDataPath`. При повторном запуске с тем же путём данные сохраняются.

---

## 3. Загрузка логов

VictoriaLogs принимает логи через HTTP в нескольких форматах: JSON Lines, Loki, Elasticsearch, OpenTelemetry, Datadog, Splunk, Syslog. Для быстрого старта удобнее всего JSON Lines.

### Формат запроса

```
POST /insert/jsonline?_stream_fields=<поля_потока>
Content-Type: application/json

{"_time":"<RFC3339>","_msg":"<текст лога>","field1":"val1",...}
{"_time":"...","_msg":"..."}
```

**`_stream_fields`** — поля, по которым VictoriaLogs группирует логи в потоки (streams). Поток — это уникальная комбинация значений этих полей, аналог лейблов в Loki. Данные одного потока хранятся вместе, что ускоряет поиск с фильтром по ним.

**`_time`** — временна́я метка в RFC3339 (или Unix timestamp). Если не указать, будет использовано время приёма.

**`_msg`** — основной текст записи. Обязательное поле.

### Пример: nginx access logs

```bash
curl -X POST 'http://localhost:9428/insert/jsonline?_stream_fields=service,host,level' \
  -H 'Content-Type: application/json' \
  --data-binary '
{"_time":"2026-07-07T13:50:00Z","_msg":"GET /api/v1/users 200 45ms","service":"nginx","host":"web-01","level":"info","method":"GET","path":"/api/v1/users","status":"200","latency_ms":"45"}
{"_time":"2026-07-07T13:50:01Z","_msg":"POST /api/v1/login 200 120ms","service":"nginx","host":"web-01","level":"info","method":"POST","path":"/api/v1/login","status":"200","latency_ms":"120"}
{"_time":"2026-07-07T13:50:02Z","_msg":"GET /api/v1/orders 500 5ms","service":"nginx","host":"web-02","level":"error","method":"GET","path":"/api/v1/orders","status":"500","latency_ms":"5"}
{"_time":"2026-07-07T13:50:03Z","_msg":"GET /static/logo.png 404 2ms","service":"nginx","host":"web-01","level":"warn","method":"GET","path":"/static/logo.png","status":"404","latency_ms":"2"}
{"_time":"2026-07-07T13:50:04Z","_msg":"GET /api/v1/products 200 88ms","service":"nginx","host":"web-02","level":"info","method":"GET","path":"/api/v1/products","status":"200","latency_ms":"88"}
{"_time":"2026-07-07T13:50:05Z","_msg":"DELETE /api/v1/users/42 403 3ms","service":"nginx","host":"web-01","level":"error","method":"DELETE","path":"/api/v1/users/42","status":"403","latency_ms":"3"}
'
```

### Пример: логи приложения (с trace_id и user_id)

```bash
curl -X POST 'http://localhost:9428/insert/jsonline?_stream_fields=service,host,level' \
  -H 'Content-Type: application/json' \
  --data-binary '
{"_time":"2026-07-07T13:50:02Z","_msg":"database connection failed: dial tcp 10.0.0.5:5432: connect: connection refused","service":"orders-api","host":"app-01","level":"error","trace_id":"abc123","db_host":"10.0.0.5"}
{"_time":"2026-07-07T13:50:02Z","_msg":"retrying database connection, attempt 1/3","service":"orders-api","host":"app-01","level":"warn","trace_id":"abc123"}
{"_time":"2026-07-07T13:50:03Z","_msg":"retrying database connection, attempt 2/3","service":"orders-api","host":"app-01","level":"warn","trace_id":"abc123"}
{"_time":"2026-07-07T13:50:04Z","_msg":"database connection established","service":"orders-api","host":"app-01","level":"info","trace_id":"abc123","db_host":"10.0.0.5"}
{"_time":"2026-07-07T13:50:10Z","_msg":"user authenticated successfully","service":"auth-service","host":"app-02","level":"info","trace_id":"def456","user_id":"42","email":"alice@example.com"}
{"_time":"2026-07-07T13:50:11Z","_msg":"payment processing started","service":"payments","host":"app-03","level":"info","trace_id":"ghi789","user_id":"42","amount":"1500","currency":"RUB"}
{"_time":"2026-07-07T13:50:12Z","_msg":"payment gateway timeout after 3000ms","service":"payments","host":"app-03","level":"error","trace_id":"ghi789","user_id":"42","gateway":"stripe"}
{"_time":"2026-07-07T13:50:13Z","_msg":"payment failed, rolling back order","service":"payments","host":"app-03","level":"error","trace_id":"ghi789","user_id":"42"}
{"_time":"2026-07-07T13:50:20Z","_msg":"cache miss for key user:42:profile","service":"orders-api","host":"app-01","level":"debug","trace_id":"xyz999","user_id":"42"}
{"_time":"2026-07-07T13:50:21Z","_msg":"slow query detected: SELECT * FROM orders WHERE user_id=42 took 2340ms","service":"orders-api","host":"app-01","level":"warn","trace_id":"xyz999","user_id":"42","query_ms":"2340"}
'
```

### Пример: Kubernetes / инфраструктура

```bash
curl -X POST 'http://localhost:9428/insert/jsonline?_stream_fields=service,host,level' \
  -H 'Content-Type: application/json' \
  --data-binary '
{"_time":"2026-07-07T13:49:00Z","_msg":"pod orders-api-7d4b9c-xvz started","service":"kubelet","host":"node-01","level":"info","namespace":"production","pod":"orders-api-7d4b9c-xvz"}
{"_time":"2026-07-07T13:49:30Z","_msg":"pod payments-6f8d-abc OOMKilled, exit code 137","service":"kubelet","host":"node-02","level":"error","namespace":"production","pod":"payments-6f8d-abc","exit_code":"137"}
{"_time":"2026-07-07T13:50:00Z","_msg":"pod payments-6f8d-xyz restarted (restartCount=3)","service":"kubelet","host":"node-02","level":"warn","namespace":"production","pod":"payments-6f8d-xyz","restart_count":"3"}
{"_time":"2026-07-07T13:51:00Z","_msg":"node node-03 NotReady: NetworkPlugin not ready","service":"kubelet","host":"node-03","level":"error","namespace":"kube-system"}
'
```

---

## 4. Поиск логов (LogsQL)

Все запросы идут на `/select/logsql/query`. Параметр `query` содержит LogsQL-выражение.

```bash
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=<выражение>'
```

Каждая строка ответа — JSON-объект с полями записи (`_time`, `_msg`, плюс все пользовательские поля).

---

### 4.1 Полнотекстовый поиск

Ищет слово в `_msg` и во всех полях:

```bash
# Всё про "connection"
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=connection'
```

```
13:50:02  orders-api  database connection failed: dial tcp 10.0.0.5:5432: ...
13:50:02  orders-api  retrying database connection, attempt 1/3
13:50:03  orders-api  retrying database connection, attempt 2/3
13:50:04  orders-api  database connection established
```

---

### 4.2 Фильтр по конкретному полю

```bash
# Все записи с level=error
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=level:error'
```

```
13:50:02  orders-api  database connection failed: ...
13:50:12  payments    payment gateway timeout after 3000ms
13:50:13  payments    payment failed, rolling back order
13:51:00  kubelet     node node-03 NotReady: NetworkPlugin not ready
13:49:30  kubelet     pod payments-6f8d-abc OOMKilled, exit code 137
13:50:02  nginx       GET /api/v1/orders 500 5ms
13:50:05  nginx       DELETE /api/v1/users/42 403 3ms
```

---

### 4.3 Комбинирование условий (AND, OR)

```bash
# Ошибки только в payments
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=service:payments AND level:error'
```

```
13:50:12  payment gateway timeout after 3000ms
13:50:13  payment failed, rolling back order
```

```bash
# OOMKilled или рестарты подов
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=service:kubelet AND (_msg:OOMKilled OR _msg:restarted)'
```

```
13:49:30  error  payments-6f8d-abc  pod payments-6f8d-abc OOMKilled, exit code 137
13:50:00  warn   payments-6f8d-xyz  pod payments-6f8d-xyz restarted (restartCount=3)
```

---

### 4.4 Трассировка запроса по trace_id

Все события одного запроса через все сервисы в хронологическом порядке:

```bash
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=trace_id:abc123 | sort by(_time)'
```

```
13:50:02  error  orders-api  database connection failed: dial tcp 10.0.0.5:5432: ...
13:50:02  warn   orders-api  retrying database connection, attempt 1/3
13:50:03  warn   orders-api  retrying database connection, attempt 2/3
13:50:04  info   orders-api  database connection established
```

---

### 4.5 История действий пользователя

```bash
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=user_id:42 | sort by(_time)'
```

```
13:50:10  auth-service  info   user authenticated successfully
13:50:11  payments      info   payment processing started
13:50:12  payments      error  payment gateway timeout after 3000ms
13:50:13  payments      error  payment failed, rolling back order
13:50:20  orders-api    debug  cache miss for key user:42:profile
13:50:21  orders-api    warn   slow query detected: ... took 2340ms
```

---

### 4.6 Поиск по regexp

```bash
# HTTP 5xx
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=service:nginx AND status:~"5.."'
```

```
13:50:02  GET  /api/v1/orders  500  GET /api/v1/orders 500 5ms
```

---

### 4.7 Числовой диапазон

```bash
# Запросы медленнее 100ms
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=service:nginx AND latency_ms:>100'
```

```
13:50:01  /api/v1/login  120ms
```

Операторы: `>`, `>=`, `<`, `<=`, диапазон `[100, 200]`.

---

### 4.8 Фильтр по стриму `{}`

Стримы — это индексированные метаданные. Запросы по стримам быстрее, чем по обычным полям, так как используют отдельный индекс:

```bash
# Только логи с host=web-01
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query={host="web-01"}'
```

```
13:50:03  GET   /static/logo.png   404
13:50:05  DELETE /api/v1/users/42  403
13:50:00  GET   /api/v1/users      200
13:50:01  POST  /api/v1/login      200
```

Фильтр `{}` работает только по полям, перечисленным в `_stream_fields` при вставке.

---

### 4.9 Агрегация через пайплайны (`|`)

LogsQL поддерживает цепочки операторов через `|`. После фильтра можно добавить агрегацию, сортировку, форматирование.

```bash
# Количество событий по каждому сервису
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=* | stats by(service) count() as total | sort by(total desc)'
```

```
orders-api   6
nginx        6
kubelet      4
payments     3
auth-service 1
```

```bash
# Ошибки и предупреждения — разбивка по сервису и уровню
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=level:error OR level:warn | stats by(service, level) count() as cnt | sort by(cnt desc)'
```

```
orders-api  warn   3
payments    error  2
kubelet     error  2
nginx       error  2
orders-api  error  1
nginx       warn   1
kubelet     warn   1
```

---

### 4.10 Извлечение поля из текста + фильтр

Если нужное значение зашито в строку `_msg` (а не выделено в отдельное поле), его можно извлечь через `| extract`:

```bash
# Найти все slow queries с временем > 1000ms
curl 'http://localhost:9428/select/logsql/query' \
  --data-urlencode 'query=service:orders-api AND query_ms:* | extract "took <query_ms_val>ms" | filter query_ms_val:>1000 | fields _time, _msg, query_ms_val'
```

```
13:50:21  2340ms  slow query detected: SELECT * FROM orders WHERE user_id=42 took 2340ms
```

Шаблон `"took <query_ms_val>ms"` извлекает число между `took` и `ms` в новое поле `query_ms_val`, после чего по нему можно фильтровать.

---

## 5. Вспомогательные API

### Список стримов и их количество

```bash
curl 'http://localhost:9428/select/logsql/streams?query=*&limit=20'
```

Возвращает все уникальные комбинации stream fields с числом попаданий — удобно для понимания структуры данных.

### Уникальные значения поля

```bash
curl 'http://localhost:9428/select/logsql/field_values?query=*&field=service'
```

```json
{"values":[
  {"value":"nginx","hits":6},
  {"value":"orders-api","hits":6},
  {"value":"kubelet","hits":4},
  {"value":"payments","hits":3},
  {"value":"auth-service","hits":1}
]}
```

Полезно для автодополнения в дашбордах или проверки того, какие значения реально есть в данных.

### Список полей

```bash
curl 'http://localhost:9428/select/logsql/field_names?query=*'
```

Возвращает все поля, встречающиеся в записях, соответствующих запросу.

---

## 6. Шпаргалка по LogsQL

| Что нужно | Синтаксис |
|-----------|-----------|
| Все записи | `*` |
| Полнотекстовый поиск | `connection` |
| Точное значение поля | `level:error` |
| Логическое И | `service:nginx AND level:error` |
| Логическое ИЛИ | `level:error OR level:warn` |
| Отрицание | `NOT level:debug` |
| Фраза | `_msg:"connection refused"` |
| Regexp поля | `status:~"5.."` |
| Числовой диапазон | `latency_ms:>100` |
| Фильтр по стриму | `{host="web-01",service="nginx"}` |
| Сортировка | `* \| sort by(_time)` |
| Агрегация | `* \| stats by(service) count()` |
| Выбор полей | `* \| fields _time, _msg, service` |
| Извлечение из текста | `* \| extract "took <val>ms"` |
| Фильтр после extract | `... \| filter val:>1000` |
