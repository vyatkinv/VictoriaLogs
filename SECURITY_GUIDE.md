# SECURITY_GUIDE.md

Гайд по настройкам безопасности VictoriaLogs: TLS, аутентификация, мультитенантность, сетевая изоляция и защита от перегрузок.

---

## Содержание

1. [TLS для HTTP-сервера](#1-tls-для-http-сервера)
2. [TLS для syslog (TCP)](#2-tls-для-syslog-tcp)
3. [TLS для кластерного трафика](#3-tls-для-кластерного-трафика)
4. [Аутентификация клиентов](#4-аутентификация-клиентов)
5. [Защита отдельных эндпоинтов через authKey](#5-защита-отдельных-эндпоинтов-через-authkey)
6. [Мультитенантность](#6-мультитенантность)
7. [Изоляция ролей в кластере](#7-изоляция-ролей-в-кластере)
15. [Управление TLS через HashiCorp Vault PKI](#15-управление-tls-через-hashicorp-vault-pki)
8. [Защита от перегрузок (ресурсные лимиты)](#8-защита-от-перегрузок-ресурсные-лимиты)
9. [Управление секретами](#9-управление-секретами)
10. [HTTP-заголовки безопасности](#10-http-заголовки-безопасности)
11. [Шифрование данных в покое](#11-шифрование-данных-в-покое)
12. [Аутентификация vlagent → VictoriaLogs](#12-аутентификация-vlagent--victorialogs)
13. [Чего нет из коробки](#13-чего-нет-из-коробки)
14. [Матрица флагов безопасности](#14-матрица-флагов-безопасности)

---

## 1. TLS для HTTP-сервера

**Реализация:** `vendor/github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver/httpserver.go`

VictoriaLogs поддерживает TLS (HTTPS) на HTTP-порту. По умолчанию сервер слушает на HTTP. Все флаги ниже могут быть заданы отдельно для каждого `-httpListenAddr` (через индексирование).

### Флаги

```
-tls                        # включить TLS для -httpListenAddr
-tlsCertFile=<path>         # путь к TLS-сертификату (ECDSA быстрее RSA)
-tlsKeyFile=<path>          # путь к TLS-ключу
-tlsMinVersion=<ver>        # минимальная версия: TLS10, TLS11, TLS12, TLS13
-tlsCipherSuites=<list>     # список разрешённых шифр-сьютов (через запятую)
```

### Важные детали

- **Сертификаты перечитываются автоматически каждую секунду.** Это означает, что ротация сертификатов (например, через cert-manager или certbot) не требует перезапуска процесса.
- HTTP/2 **отключён** намеренно (строка `TLSNextProto: make(map[string]func(...))`). Используется HTTP/1.1 поверх TLS.
- Рекомендуется использовать ECDSA-сертификаты: они быстрее при TLS handshake, чем RSA.
- Флаг `-tlsCertFile` упоминает `-tlsAutocertHosts` как альтернативу — это ссылка на функциональность VictoriaMetrics Enterprise (автоматическое получение сертификатов через ACME/Let's Encrypt). В open-source версии VictoriaLogs эта функция **недоступна**.

### Пример запуска с TLS

```bash
./victoria-logs \
  -httpListenAddr=:9428 \
  -tls \
  -tlsCertFile=/etc/ssl/certs/vl.crt \
  -tlsKeyFile=/etc/ssl/private/vl.key \
  -tlsMinVersion=TLS13
```

### Несколько адресов с разными настройками TLS

Флаги `-tls`, `-tlsCertFile`, `-tlsKeyFile` поддерживают множественные значения через запятую — по одному на каждый `-httpListenAddr`:

```bash
./victoria-logs \
  -httpListenAddr=:9428,:9429 \
  -tls=true,false \
  -tlsCertFile=/certs/internal.crt, \
  -tlsKeyFile=/certs/internal.key,
```

Здесь `:9428` получит TLS, а `:9429` — обычный HTTP.

---

## 2. TLS для syslog (TCP)

**Реализация:** `app/vlinsert/syslog/syslog.go`

Syslog-приёмник имеет **собственные** TLS-флаги, не зависящие от HTTP-сервера. По умолчанию минимальная версия — **TLS 1.3** (строже, чем для HTTP).

### Флаги

```
-syslog.tls[N]              # включить TLS для N-го -syslog.listenAddr.tcp
-syslog.tlsCertFile[N]      # TLS-сертификат
-syslog.tlsKeyFile[N]       # TLS-ключ
-syslog.tlsCipherSuites[N]  # шифр-сьюты
-syslog.tlsMinVersion       # минимальная версия (по умолчанию: TLS13)
```

Поддерживается несколько адресов с независимыми TLS-настройками (индексирование через `[N]`).

UDP и Unix-сокеты TLS **не поддерживают** — только TCP.

```bash
./victoria-logs \
  -syslog.listenAddr.tcp=:6514 \
  -syslog.tls \
  -syslog.tlsCertFile=/etc/ssl/syslog.crt \
  -syslog.tlsKeyFile=/etc/ssl/syslog.key
```

---

## 3. TLS для кластерного трафика

**Реализация:** `app/vlstorage/main.go`

В кластерном режиме insert/select-узлы общаются с storage-узлами. По умолчанию трафик между ними **не зашифрован**. Для включения TLS нужно настроить флаги на стороне клиента (vlinsert/vlselect-узлов).

### Флаги на стороне клиента (на insert/select-узле)

```
-storageNode.tls[N]                   # использовать TLS к N-му -storageNode
-storageNode.tlsCAFile[N]             # CA-сертификат для проверки storage-узла
-storageNode.tlsCertFile[N]           # клиентский TLS-сертификат (mTLS)
-storageNode.tlsKeyFile[N]            # клиентский TLS-ключ (mTLS)
-storageNode.tlsServerName[N]         # ожидаемое имя сервера в сертификате
-storageNode.tlsInsecureSkipVerify[N] # пропустить проверку сертификата (небезопасно)
```

### Мутуальный TLS (mTLS) между узлами кластера

Задав `-storageNode.tlsCertFile` и `-storageNode.tlsKeyFile`, insert/select-узлы будут предъявлять клиентский сертификат при подключении к storage. Storage-узел, в свою очередь, должен быть настроен с TLS через `-tls`, `-tlsCertFile`, `-tlsKeyFile`.

**Важно:** mTLS аутентифицирует *узлы кластера* друг с другом, но не конечных пользователей API.

---

## 4. Аутентификация клиентов

**Реализация:** `vendor/.../lib/httpserver/httpserver.go:CheckBasicAuth()`

### HTTP Basic Auth (глобальная)

Включается парой флагов. Когда задан `httpAuth.username`, **все** HTTP-запросы (кроме `/health`, `/ping`, `/-/healthy`, `/-/ready`, `/robots.txt`) проверяются на наличие корректных Basic Auth-реквизитов.

```
-httpAuth.username=<user>
-httpAuth.password=<password>
```

**Как проверяется:** заголовок `Authorization: Basic <base64(user:pass)>`. При несоответствии возвращается `401 Unauthorized` + заголовок `WWW-Authenticate`.

**Что не защищается Basic Auth:**
- `/health`, `/ping`, `/-/healthy`, `/-/ready` — всегда открыты (нужны load balancer'ам)
- Эндпоинты с собственными authKey (`/metrics`, `/flags`, `/debug/pprof/`, `/internal/*`) — проверяются через authKey, а не Basic Auth

**Пример:**
```bash
./victoria-logs -httpAuth.username=admin -httpAuth.password=secret
# Запрос
curl -u admin:secret http://localhost:9428/select/logsql/query?query=...
```

---

## 5. Защита отдельных эндпоинтов через authKey

**Реализация:** `vendor/.../lib/httpserver/httpserver.go:CheckAuthFlag()`

Отдельные чувствительные эндпоинты защищены независимыми ключами. authKey передаётся как **query-параметр** `?authKey=<value>`.

### Встроенные (httpserver) authKey

| Флаг | Защищаемый эндпоинт |
|------|---------------------|
| `-metricsAuthKey` | `/metrics` |
| `-flagsAuthKey` | `/flags` |
| `-pprofAuthKey` | `/debug/pprof/*` |

### authKey специфичные для VictoriaLogs (`app/vlstorage/main.go`)

| Флаг | Защищаемый эндпоинт |
|------|---------------------|
| `-logNewStreamsAuthKey` | `/internal/log_new_streams` |
| `-forceMergeAuthKey` | `/internal/force_merge` |
| `-forceFlushAuthKey` | `/internal/force_flush` |
| `-partitionManageAuthKey` | `/internal/partition/*` |

### Логика `CheckAuthFlag`

```
если authKey-флаг установлен:
  ├─ запрос без ?authKey → 401
  └─ запрос с неверным ?authKey → 401
если authKey-флаг НЕ установлен:
  └─ проверяется -httpAuth.* (Basic Auth)
```

Таким образом, authKey **переопределяет** Basic Auth для конкретного эндпоинта.

### Эндпоинты удаления (opt-in)

Эндпоинты удаления логов отключены по умолчанию и требуют явного включения:

```
-delete.enable           # включить /delete/* (run_task, stop_task, active_tasks)
-internaldelete.enable   # включить /internal/delete/* (для кластера)
```

### Практические рекомендации

- Всегда задавать `-metricsAuthKey`, `-flagsAuthKey`, `-pprofAuthKey` в production — иначе любой, у кого есть HTTP-доступ, может читать метрики и дампы профилировщика.
- `/debug/pprof/` особенно опасен: позволяет снять heap-дамп, содержащий данные логов в памяти.
- authKey передаётся в URL, поэтому **без TLS** он виден в открытом трафике и в логах балансировщиков.

---

## 6. Мультитенантность

**Реализация:** `lib/logstorage/tenant_id.go`

VictoriaLogs поддерживает изоляцию данных по тенантам. Каждая запись хранится с `TenantID{AccountID uint32, ProjectID uint32}`.

### Как тенант задаётся при записи

TenantID извлекается из HTTP-заголовков запроса:

```
AccountID: 42
ProjectID: 7
```

Если заголовки отсутствуют — используется `TenantID{0, 0}` (тенант по умолчанию).

```go
// lib/logstorage/tenant_id.go:GetTenantIDFromRequest
accountID = r.Header.Get("AccountID")
projectID = r.Header.Get("ProjectID")
```

### Как тенант задаётся при запросе

Аналогично — через заголовки `AccountID` и `ProjectID` в запросе к `/select/logsql/query` и другим select-эндпоинтам.

### Важное ограничение

**VictoriaLogs не выполняет авторизацию по тенантам.** Заголовки `AccountID`/`ProjectID` доверенны — любой клиент может запросить данные любого тенанта. Для реальной мультитенантной изоляции необходим внешний прокси (например, vmauth), который:
1. Аутентифицирует пользователя.
2. Проставляет правильные `AccountID`/`ProjectID` заголовки.
3. Не позволяет пользователю самостоятельно переопределить эти заголовки.

### vlogscli и тенанты

```bash
./vlogscli -accountID=42 -projectID=7 -datasource.url=http://localhost:9428/select/logsql/query
```

---

## 7. Изоляция ролей в кластере

**Реализация:** `app/vlinsert/main.go`, `app/vlselect/main.go`

В кластерном режиме каждый узел может быть ограничен только нужной ролью.

### Флаги изоляции

| Флаг | Эффект |
|------|--------|
| `-insert.disable` | Отключить `/insert/*` и `/internal/insert` |
| `-internalinsert.disable` | Отключить только `/internal/insert` (межузловой трафик) |
| `-select.disable` | Отключить `/select/*` и `/internal/select/*` |
| `-internalselect.disable` | Отключить только `/internal/select/*` |

### Типичные конфигурации

**Dedicated insert-узел (принимает данные снаружи, запросы — нет):**
```bash
-select.disable
```

**Dedicated select-узел (отвечает на запросы, данные — нет):**
```bash
-insert.disable
```

**Storage-узел (только внутренние операции, нет внешнего трафика):**
```bash
-insert.disable -select.disable
# только /internal/insert и /internal/select/* остаются доступны
```

Затем для storage-узла отключают и внутренние эндпоинты для внешних клиентов, выставляя его только во внутренней сети.

---

## 8. Защита от перегрузок (ресурсные лимиты)

### Лимиты запросов

```
-search.maxConcurrentRequests=N    # максимум параллельных запросов (default: 2*CPU, не более 16)
-search.maxQueueDuration=10s       # время ожидания в очереди при достижении лимита
-search.maxQueryDuration=30s       # максимальная длительность одного запроса
-search.maxQueryTimeRange=0        # максимальный time range в одном запросе (0 = без ограничения)
```

### Лимиты размера запросов на запись

Каждый протокол имеет свой лимит по умолчанию:

| Протокол | Флаг | Default |
|----------|------|---------|
| Loki | `-loki.maxRequestSize` | 64 МБ |
| Elasticsearch | нет отдельного флага | — |
| OpenTelemetry | `-opentelemetry.maxRequestSize` | 64 МБ |
| Datadog | `-datadog.maxRequestSize` | 64 МБ |
| Splunk | `-splunk.maxRequestSize` | 64 МБ |
| Native | `-nativeinsert.maxRequestSize` | 64 МБ |
| Internal | `-internalinsert.maxRequestSize` | 64 МБ |
| JSON line | `-insert.maxLineSizeBytes` | 256 КБ на строку |

### Лимиты полей

```
-insert.maxFieldsPerLine=1000    # максимум полей в одной записи
```

Записи с числом полей > лимита обрезаются (лишние поля отбрасываются).

### Минимальное свободное место на диске

```
-storage.minFreeDiskSpaceBytes=10MB
```

При достижении порога хранилище переходит в read-only режим и прекращает принимать новые данные.

---

## 9. Управление секретами

**Реализация:** `vendor/.../lib/flagutil/password.go`

Тип `Password` (используется для всех чувствительных флагов: authKey, пароли, токены) имеет специальную обработку:

### Три способа задать секрет

**1. Прямо в аргументе (не рекомендуется в production):**
```bash
-httpAuth.password=mysecret
```

**2. Из файла (перечитывается каждые 2 секунды):**
```bash
-httpAuth.password=file:///etc/secrets/vl-password
# или относительный путь:
-httpAuth.password=file://./secrets/password
```

**3. Из HTTP/HTTPS URL (перечитывается каждые 2 секунды):**
```bash
-httpAuth.password=https://vault.internal/v1/secret/vl-password
```

### Защита от утечки

- `Password.String()` всегда возвращает строку `"secret"` — значение никогда не попадает в логи или на `/flags`.
- authKey автоматически маскируется в логах при записи URL (`r.Form.Encode()` скрывает его через `flagutil.RegisterSecretFlag`).
- При инициализации из файла или URL сразу генерируется случайный 64-байтовый пароль — защита от гонки при старте, пока файл ещё не прочитан.

### Переменные окружения

Флаги можно задать через переменные окружения, включив:
```bash
-envflag.enable
-envflag.prefix=VL_   # необязательно
```

Тогда флаг `-httpAuth.password` читается из env `VL_HTTPAUTH_PASSWORD` (точки заменяются на подчёркивания).

Для пометки произвольного флага как секретного:
```bash
-secret.flags=httpAuth.password,metricsAuthKey
```

---

## 10. HTTP-заголовки безопасности

**Реализация:** `vendor/.../lib/httpserver/httpserver.go:handlerWrapper()`

Следующие заголовки применяются ко **всем** HTTP-ответам, если флаги заданы:

| Флаг | HTTP-заголовок | Рекомендованное значение |
|------|----------------|--------------------------|
| `-http.header.hsts` | `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` |
| `-http.header.frameOptions` | `X-Frame-Options` | `DENY` или `SAMEORIGIN` |
| `-http.header.csp` | `Content-Security-Policy` | `default-src 'self'` |
| `-http.header.disableServerHostname` | убирает `X-Server-Hostname` | `true` |

По умолчанию все эти заголовки **не выставляются**. При доступе VMUI через браузер рекомендуется задать хотя бы CSP и X-Frame-Options.

```bash
./victoria-logs \
  -http.header.hsts="max-age=31536000; includeSubDomains" \
  -http.header.frameOptions=DENY \
  -http.header.csp="default-src 'self'" \
  -http.header.disableServerHostname=true
```

### CORS

По умолчанию VMUI и select-эндпоинты разрешают CORS от любых источников (`Access-Control-Allow-Origin: *`). Это можно отключить:
```bash
-http.disableCORS=true
```

---

## 11. Шифрование данных в покое

**VictoriaLogs не реализует встроенное шифрование данных на диске.** Данные в партициях хранятся в открытом виде (сжатые zstd, но не зашифрованные).

Для шифрования данных в покое необходимо использовать внешние средства:
- **Linux dm-crypt / LUKS** — шифрование на уровне блочного устройства
- **eCryptfs / fscrypt** — шифрование на уровне файловой системы
- **Encrypted EBS / Cloud Storage** — шифрование управляемых дисков у облачных провайдеров

Эти подходы прозрачны для VictoriaLogs и не требуют изменений в конфигурации.

---

## 12. Аутентификация vlagent → VictoriaLogs

**Реализация:** `app/vlagent/remotewrite/client.go`

vlagent отправляет данные на VictoriaLogs через `-remoteWrite.url`. Поддерживаемые методы аутентификации:

### Basic Auth
```bash
-remoteWrite.basicAuth.username=vlagent
-remoteWrite.basicAuth.password=secret
# или из файла:
-remoteWrite.basicAuth.passwordFile=/etc/secrets/vlagent-password
```

### Bearer Token
```bash
-remoteWrite.bearerToken=my-token
# или из файла (перечитывается каждую секунду):
-remoteWrite.bearerTokenFile=/var/run/secrets/token
```

### OAuth2
```bash
-remoteWrite.oauth2.clientID=my-client
-remoteWrite.oauth2.clientSecretFile=/etc/oauth2/secret
-remoteWrite.oauth2.tokenUrl=https://auth.example.com/token
-remoteWrite.oauth2.scopes=logs:write
```

### TLS для vlagent → VictoriaLogs
```bash
-remoteWrite.tlsCAFile=/etc/ssl/ca.crt       # проверка сертификата сервера
-remoteWrite.tlsCertFile=/etc/ssl/client.crt # клиентский сертификат (mTLS)
-remoteWrite.tlsKeyFile=/etc/ssl/client.key  # клиентский ключ (mTLS)
-remoteWrite.tlsServerName=vl.internal       # ожидаемый SNI
-remoteWrite.tlsHandshakeTimeout=20s
```

### Произвольные заголовки
```bash
# Для передачи API-ключа через кастомный заголовок:
-remoteWrite.headers='X-Auth-Token: my-token'
# Несколько заголовков:
-remoteWrite.headers='X-Tenant: 42^^X-Project: 7'
```

---

## 13. Чего нет из коробки

| Функциональность | Состояние | Альтернатива |
|-----------------|-----------|--------------|
| Авторизация по тенантам | Нет | Внешний прокси (vmauth, nginx) |
| RBAC (роли и права) | Нет | Внешний прокси |
| JWT / OIDC на стороне сервера | Нет | Внешний прокси |
| Шифрование данных в покое | Нет | OS-level encryption (LUKS, dm-crypt) |
| Аудит-лог HTTP-запросов | Нет | nginx/envoy access logs перед VL |
| mTLS для входящих клиентских подключений | Нет | Реализовано только между узлами кластера |
| TLS Autocert (Let's Encrypt) | Enterprise | Ручная ротация сертификатов |
| Rate limiting по клиентскому IP | Нет | Внешний rate limiter |
| IP allowlist/blocklist | Нет | firewall / nginx `allow`/`deny` |

---

## 14. Матрица флагов безопасности

Сводная таблица всех security-значимых флагов:

### TLS — входящий трафик

| Флаг | Компонент | Описание |
|------|-----------|----------|
| `-tls` | HTTP-сервер | Включить HTTPS |
| `-tlsCertFile` | HTTP-сервер | Путь к сертификату |
| `-tlsKeyFile` | HTTP-сервер | Путь к ключу |
| `-tlsMinVersion` | HTTP-сервер | Минимальная версия TLS |
| `-tlsCipherSuites` | HTTP-сервер | Разрешённые шифр-сьюты |
| `-syslog.tls` | Syslog TCP | Включить TLS для syslog |
| `-syslog.tlsCertFile` | Syslog TCP | Сертификат syslog |
| `-syslog.tlsKeyFile` | Syslog TCP | Ключ syslog |
| `-syslog.tlsMinVersion` | Syslog TCP | Минимальная версия (default: TLS13) |

### TLS — исходящий трафик (кластер)

| Флаг | Описание |
|------|----------|
| `-storageNode.tls` | TLS к storage-узлам |
| `-storageNode.tlsCAFile` | CA для проверки storage |
| `-storageNode.tlsCertFile` | Клиентский сертификат (mTLS) |
| `-storageNode.tlsKeyFile` | Клиентский ключ (mTLS) |
| `-storageNode.tlsServerName` | Ожидаемый SNI |
| `-storageNode.tlsInsecureSkipVerify` | Пропустить проверку (небезопасно) |

### Аутентификация

| Флаг | Описание |
|------|----------|
| `-httpAuth.username` | Basic Auth: пользователь |
| `-httpAuth.password` | Basic Auth: пароль |
| `-metricsAuthKey` | authKey для `/metrics` |
| `-flagsAuthKey` | authKey для `/flags` |
| `-pprofAuthKey` | authKey для `/debug/pprof/*` |
| `-logNewStreamsAuthKey` | authKey для `/internal/log_new_streams` |
| `-forceMergeAuthKey` | authKey для `/internal/force_merge` |
| `-forceFlushAuthKey` | authKey для `/internal/force_flush` |
| `-partitionManageAuthKey` | authKey для `/internal/partition/*` |

### Изоляция эндпоинтов

| Флаг | Описание |
|------|----------|
| `-insert.disable` | Отключить запись (`/insert/*`, `/internal/insert`) |
| `-internalinsert.disable` | Отключить только межузловую запись |
| `-select.disable` | Отключить запросы (`/select/*`, `/internal/select/*`) |
| `-internalselect.disable` | Отключить только межузловые запросы |
| `-delete.enable` | Включить удаление логов (по умолчанию: off) |
| `-internaldelete.enable` | Включить межузловое удаление |

### HTTP-заголовки безопасности

| Флаг | Описание |
|------|----------|
| `-http.header.hsts` | Заголовок HSTS |
| `-http.header.frameOptions` | X-Frame-Options |
| `-http.header.csp` | Content-Security-Policy |
| `-http.header.disableServerHostname` | Убрать X-Server-Hostname |
| `-http.disableCORS` | Отключить CORS |

### Ресурсные лимиты

| Флаг | Описание |
|------|----------|
| `-search.maxConcurrentRequests` | Лимит параллельных запросов |
| `-search.maxQueryDuration` | Таймаут выполнения запроса |
| `-search.maxQueryTimeRange` | Максимальный time range в запросе |
| `-insert.maxLineSizeBytes` | Максимальная длина одной строки |
| `-insert.maxFieldsPerLine` | Максимум полей в записи |
| `-storage.minFreeDiskSpaceBytes` | Порог read-only при нехватке диска |
| `-loki.maxRequestSize` | Максимальный размер Loki-запроса |
| `-datadog.maxRequestSize` | Максимальный размер Datadog-запроса |
| `-opentelemetry.maxRequestSize` | Максимальный размер OTel-запроса |
| `-splunk.maxRequestSize` | Максимальный размер Splunk-запроса |

### Управление секретами

| Флаг | Описание |
|------|----------|
| `-envflag.enable` | Разрешить читать флаги из env-переменных |
| `-envflag.prefix` | Префикс для env-переменных |
| `-secret.flags` | Список флагов для маскировки в логах |

---

## 15. Управление TLS через HashiCorp Vault PKI

VictoriaLogs поддерживает получение TLS-сертификатов напрямую из [Vault PKI Secrets Engine](https://developer.hashicorp.com/vault/docs/secrets/pki) с автоматическим горячим обновлением без перезапуска процесса.

### Зачем это нужно

При использовании файловых сертификатов (`-tlsCertFile / -tlsKeyFile`) жизненный цикл управляется вручную: нужно своевременно перевыпускать файлы и следить за ротацией. Vault PKI решает это централизованно:

- короткоживущие сертификаты (минуты/часы) без ручного вмешательства;
- единый источник PKI для всех сервисов;
- отзыв (revocation) через CRL/OCSP;
- аудит каждого выпуска.

### Архитектура реализации

```
┌──────────────────────────────────────────────────────────────────────┐
│ victoria-logs                                                          │
│                                                                        │
│  main()                                                                │
│    ├─ initVaultTLS()          [app/victoria-logs/vault_tls.go]         │
│    │    ├─ vaulttls.NewProvider(cfg)  [lib/vaulttls/vaulttls.go]       │
│    │    │    ├─ os.MkdirTemp() → cert.pem, key.pem   (для HTTP)        │
│    │    │    ├─ renew()  ────────────────────────────────────────────┼──► POST /v1/pki/issue/role
│    │    │    │    ├─ tls.X509KeyPair → p.cert  (в память, для syslog) │    Vault HTTP API
│    │    │    │    └─ writePEMFiles(cert, key)  атомарно на диск       │
│    │    │    └─ go backgroundRenewer() → renew() каждые lifetime·2/3  │
│    │    ├─ vaulttls.Register(p)              (syslog: in-memory)       │
│    │    ├─ flag.Set("tls", "true")                                     │
│    │    ├─ flag.Set("tlsCertFile", provider.CertFile())  (HTTP: файлы) │
│    │    └─ flag.Set("tlsKeyFile",  provider.KeyFile())                 │
│    │                                                                   │
│    ├─ vlinsert.Init() → syslog.runTCPListener()                        │
│    │    └─ vaulttls.ServerTLSConfig() → cfg.GetCertificate = p.GetCert │  ← наш код
│    │                                                                   │
│    └─ httpserver.Serve()                                               │
│         └─ netutil.GetServerTLSConfig(cert.pem, key.pem, ...)         │  ← vendor без изменений
│              └─ cfg.GetCertificate = newGetCertificateFunc(...)        │
│                   (перечитывает файлы ~раз в секунду)                  │
└──────────────────────────────────────────────────────────────────────┘
```

**Два способа выдачи, оба без правок vendor.** У VictoriaLogs два TLS-слушателя, и их `tls.Config` строится в разных местах:

- **syslog** — `tls.Config` собирается в нашем `app/vlinsert/syslog/syslog.go`, поэтому он берёт `*tls.Config` с in-memory `GetCertificate` из `vaulttls.ServerTLSConfig()`. Приватный ключ syslog **не пишется на диск**.
- **HTTP** — `tls.Config` собирается внутри vendored `httpserver.Serve`, инъекции нет. Но штатный `newGetCertificateFunc` уже перечитывает cert/key-файлы примерно раз в секунду, поэтому провайдер держит PEM-файлы актуальными, а `initVaultTLS` направляет на них `-tlsCertFile`/`-tlsKeyFile`.

Ни один файл в `vendor/` не модифицируется — `make vendor-update` остаётся безопасным.

**Поток данных при каждом TLS-рукопожатии:**

- **syslog:** Go runtime вызывает `p.GetCertificate(hello)` → под мьютексом отдаётся текущий `p.cert` из памяти. Фоновый renewer подменяет `p.cert` — обновление прозрачно.
- **HTTP:** vendored-колбэк раз в секунду перечитывает `cert.pem`/`key.pem` через `LoadX509KeyPair`, иначе отдаёт кэш; провайдер переписывает эти файлы до истечения.

**Горутина фонового обновления:**

```
выпуск сертификата (t=0, TTL=24h)
         │
         ├─ renewDeadline = expiry − lifetime/3 = t + 16h
         │
         └─ backgroundRenewer спит до t+16h
                   │
                   ├─ renew() → новый сертификат → перезапись PEM-файлов
                   └─ следующий цикл: спит до (t+16h)+16h ...
```

`renewDeadline` вычисляется от `issuedAt` (момент выпуска), а не от `time.Until(expiry)`. Это критично: если бы мы использовали оставшееся время, дедлайн бы уменьшался быстрее реального хода времени и горутина никогда не срабатывала бы вовремя.

**Атомарная запись файлов:**

```go
// lib/vaulttls/vaulttls.go
func writeFileAtomic(path string, data []byte) error {
    tmp := path + ".tmp"
    os.WriteFile(tmp, data, 0o600)  // приватные права
    return os.Rename(tmp, path)     // атомарная подмена
}
```

Ключ записывается раньше сертификата: если vendored-reader между двумя `rename` подхватит новый cert, соответствующий ему key уже на диске, и `LoadX509KeyPair` не упадёт на несовпадении пары. Файлы нужны только HTTP-слушателю; syslog берёт сертификат из памяти и этой проблемы не имеет.

### Быстрый старт

**Шаг 1: подготовить Vault PKI**

```bash
# Включить PKI-движок
vault secrets enable pki
vault secrets tune -max-lease-ttl=8760h pki

# Создать корневой CA
vault write pki/root/generate/internal \
    common_name="My CA" ttl=8760h

# Настроить URL'ы (для CRL)
vault write pki/config/urls \
    issuing_certificates="https://vault:8200/v1/pki/ca" \
    crl_distribution_points="https://vault:8200/v1/pki/crl"

# Создать роль для VictoriaLogs
vault write pki/roles/victoria-logs \
    allowed_domains="victorialogs.example.com,localhost" \
    allow_bare_domains=true \
    allow_subdomains=true \
    allow_ip_sans=true \
    max_ttl=72h \
    key_type=ec \
    key_bits=256
```

**Шаг 2: выдать токен с минимальными правами**

```hcl
# vault-policy-victorialogs.hcl
path "pki/issue/victoria-logs" {
  capabilities = ["update"]
}
```

```bash
vault policy write victoria-logs vault-policy-victorialogs.hcl

vault token create \
    -policy=victoria-logs \
    -period=768h \          # периодически обновлять токен через token-файл
    -display-name=victorialogs
```

**Шаг 3: запустить VictoriaLogs**

```bash
victoria-logs \
  -httpListenAddr=:9428 \
  -tls=true \
  -tls.vaultAddr=https://vault.example.com:8200 \
  -tls.vaultTokenFile=/run/secrets/vault-token \
  -tls.vaultPKIPath=pki \
  -tls.vaultRole=victoria-logs \
  -tls.vaultCommonName=victorialogs.example.com \
  -tls.vaultAltNames=localhost,127.0.0.1 \
  -tls.vaultTTL=24h
```

При старте в логах появится:

```
initialising Vault PKI TLS provider: addr=https://vault.example.com:8200, ...
vaulttls: issued certificate for CN="victorialogs.example.com", expires 2026-07-09T12:00:00Z (in 24h0m0s)
Vault PKI TLS provider ready; certificate expires at 2026-07-09T12:00:00Z
started server at https://0.0.0.0:9428/
```

Через 16 ч фоновая горутина выпишет новый сертификат без перезапуска:

```
vaulttls: issued certificate for CN="victorialogs.example.com", expires 2026-07-10T04:00:00Z (in 24h0m0s)
```

### Тест горячей замены (локальный)

Поднимает Vault в dev-режиме и VictoriaLogs с TTL=2m. Обновление происходит на ~80-й секунде:

```bash
# Запуск стека
docker compose -f docker-compose.vault.yml up -d --build

# E2E тест (занимает ~100 с)
./scripts/test-vault-tls.sh
```

Ожидаемый вывод теста:

```
=== 2. Check initial TLS certificate ===
notAfter=Jul  8 05:23:43 2026 GMT

=== 6. Verify certificate was renewed (new notAfter) ===
notAfter=Jul  8 05:25:03 2026 GMT
PASS: certificate was renewed successfully.
```

### Справочник флагов

| Флаг | По умолчанию | Описание |
|------|-------------|----------|
| `-tls.vaultAddr` | `""` | Адрес Vault, напр. `https://vault:8200`. Vault-режим активируется только когда флаг задан. |
| `-tls.vaultToken` | `""` | Статический токен. Поддерживает `file:///path` и `https://url` через механизм `Password`. |
| `-tls.vaultTokenFile` | `""` | Путь к файлу с токеном. Перечитывается при каждом обновлении — поддерживает ротацию токена. |
| `-tls.vaultPKIPath` | `pki` | Mount path PKI-движка в Vault. |
| `-tls.vaultRole` | `""` | Имя PKI-роли для выпуска сертификатов. |
| `-tls.vaultCommonName` | `""` | CN сертификата. |
| `-tls.vaultAltNames` | `""` | Дополнительные SAN через запятую (DNS и IP). |
| `-tls.vaultTTL` | `24h` | Запрашиваемый TTL. Vault применит `max_ttl` из роли если запрошено больше. |
| `-tls.vaultRenewBefore` | `0` | Насколько до истечения обновлять. По умолчанию `TTL/3`. |

### Приоритет провайдеров

```
-tls.vaultAddr задан?
    ДА  → HTTP:   Vault PKI управляет -tlsCertFile/-tlsKeyFile сам (файлы);
                  явно заданные -tlsCertFile/-tlsKeyFile → фатальная ошибка (конфликт)
          syslog: при -syslog.tls сертификат берётся из памяти Vault-провайдера;
                  явно заданные -syslog.tlsCertFile/-syslog.tlsKeyFile → фатальная ошибка
    НЕТ → файловые сертификаты (существующее поведение, горячая замена каждые 1 с)
```

syslog подключается к Vault только при явно включённом `-syslog.tls`; без него syslog-слушатель остаётся плейнтекстовым.

### Ротация токена Vault

Если Vault-токен истечёт — обновление сертификата завершится ошибкой, но текущий сертификат будет раздаваться до истечения срока:

```
vaulttls: background renewal failed: vault returned HTTP 403: ...; will retry in 10s
```

Для zero-downtime ротации токена:
1. Выпустить новый токен
2. Записать в файл `-tls.vaultTokenFile`
3. Следующий `renew()` подхватит его автоматически (файл перечитывается при каждом обращении)

### Безопасность

- **Токен в памяти:** значение `-tls.vaultToken` маскируется в логах (тип `flagutil.Password` всегда выводит `"secret"`).
- **mTLS до Vault:** если Vault сам за mTLS, передайте CA через системный пул (`ca-certificates`) внутри контейнера; кастомный TLS-клиент в `Provider` пока не реализован.
- **Нет at-rest шифрования ключа:** приватный ключ живёт в памяти процесса. Защита на уровне OS — `mlock` или seccomp, если требуется.
- **Аудит:** каждый вызов `POST /v1/pki/issue/role` фиксируется в Vault audit log с меткой токена `victoria-logs`.

