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
│    ├─ vaultflags.Init()       [lib/vaulttls/vaultflags]               │
│    │    ├─ vaulttls.NewProvider(cfg)  [lib/vaulttls/vaulttls.go]       │
│    │    │    ├─ renew() → fetchCert()                                  │
│    │    │    │    ├─ tokenSource.get()  [lib/vaulttls/auth.go]         │
│    │    │    │    │    ├─ unwrap  ───────────────────────────────────┼──► POST /v1/sys/wrapping/unwrap
│    │    │    │    │    └─ login   ───────────────────────────────────┼──► POST /v1/auth/<mount>/login
│    │    │    │    ├─ issueCert(token) ───────────────────────────────┼──► POST /v1/pki/issue/role
│    │    │    │    └─ tls.X509KeyPair → p.cert   (только в памяти)     │    Vault HTTP API
│    │    │    └─ go backgroundRenewer() → renew() каждые lifetime·2/3  │
│    │    ├─ vaulttls.Register(p)                                        │
│    │    └─ flag.Set("tls", "true")   (только схема https, без файлов)  │
│    │                                                                   │
│    ├─ vlinsert.Init() → syslog.runTCPListener()                        │
│    │    └─ vaulttls.ServerTLSConfig() → cfg.GetCertificate = p.GetCert │
│    │                                                                   │
│    └─ httpserver.Serve(..., ServeOptions{                              │
│              GetTLSConfig: vaulttls.ServerTLSConfig,                   │
│         })                                    [lib/httpserver — форк]  │
│         └─ serve(): opts.GetTLSConfig(minVersion, cipherSuites)        │
│              └─ cfg.GetCertificate = p.GetCertificate                  │
└──────────────────────────────────────────────────────────────────────┘
```

**Один способ выдачи для обоих слушателей.** И syslog, и HTTP получают один и тот же `*tls.Config` из `vaulttls.ServerTLSConfig()` — с in-memory `GetCertificate`. Приватный ключ **никогда не попадает на файловую систему**: ни на диск, ни в tmpfs.

Различается только точка вызова:

- **syslog** — `tls.Config` собирается в `app/vlinsert/syslog/syslog.go`, который вызывает `vaulttls.ServerTLSConfig()` напрямую.
- **HTTP** — `tls.Config` собирается внутри `httpserver.serve()`, который вызывает `opts.GetTLSConfig`. Это поле — единственная правка форка `lib/httpserver` относительно апстрима (помечена `VL-FORK:`). Ради неё пакет и был вынесен из `vendor/`: см. `lib/httpserver/UPSTREAM.md`.

Если `-tls.vaultAddr` не задан, `ServerTLSConfig` возвращает `(nil, nil)`, и оба слушателя штатно откатываются на файловые `-tlsCertFile`/`-tlsKeyFile`.

Файлы в `vendor/` по-прежнему не модифицируются — `make vendor-update` остаётся безопасным.

**Поток данных при каждом TLS-рукопожатии** (одинаков для обоих слушателей): Go runtime вызывает `p.GetCertificate(hello)` → под мьютексом отдаётся текущий `p.cert` из памяти. Фоновый renewer подменяет `p.cert` — обновление прозрачно и не требует ни перезапуска, ни перечитывания файлов.

**Горутина фонового обновления:**

```
выпуск сертификата (t=0, TTL=24h)
         │
         ├─ renewDeadline = expiry − lifetime/3 = t + 16h
         │
         └─ backgroundRenewer спит до t+16h
                   │
                   ├─ renew() → новый сертификат → подмена p.cert под мьютексом
                   └─ следующий цикл: спит до (t+16h)+16h ...
```

`renewDeadline` вычисляется от `issuedAt` (момент выпуска), а не от `time.Until(expiry)`. Это критично: если бы мы использовали оставшееся время, дедлайн бы уменьшался быстрее реального хода времени и горутина никогда не срабатывала бы вовремя.

**Смена сертификата атомарна по построению.** `renew()` сначала проверяет пару через `tls.X509KeyPair` и только потом присваивает `p.cert` под мьютексом. Неудачный выпуск не затирает действующий сертификат, а рукопожатие всегда видит либо старую, либо новую пару целиком — промежуточного состояния нет. Это и есть главное преимущество in-memory-подхода перед файловым: не нужны ни временные файлы, ни `rename`, ни порядок записи ключа и сертификата.

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

**Шаг 2: политика с минимальными правами**

```hcl
# vault-policy-victorialogs.hcl
path "pki/issue/victoria-logs" {
  capabilities = ["update"]
}

# Нужно только при -tls.vaultRevokeOnShutdown.
path "pki/revoke" {
  capabilities = ["update"]
}
```

```bash
vault policy write victoria-logs vault-policy-victorialogs.hcl
```

`sys/wrapping/unwrap` и `auth/token/revoke-self` уже входят в политику `default`, добавлять их не нужно.

**Шаг 3: выбрать метод аутентификации** — см. следующий раздел. Для примера ниже используется AppRole.

**Шаг 4: запустить VictoriaLogs**

```bash
victoria-logs \
  -httpListenAddr=:9428 \
  -tls=true \
  -tls.vaultAddr=https://vault.example.com:8200 \
  -tls.vaultCAFile=/etc/ssl/certs/internal-ca.pem \
  -tls.vaultAuthMethod=approle \
  -tls.vaultAuthRoleIDFile=/run/secrets/vault-role-id \
  -tls.vaultAuthSecretIDWrappedFile=/run/secrets/vault-secret-id-wrapped \
  -tls.vaultPKIPath=pki \
  -tls.vaultRole=victoria-logs \
  -tls.vaultCommonName=victorialogs.example.com \
  -tls.vaultAltNames=localhost,127.0.0.1 \
  -tls.vaultTTL=24h
```

При старте в логах появится:

```
initialising Vault PKI TLS provider: addr=https://vault.example.com:8200, ..., auth=approle
vaulttls: authenticated in vault via the "approle" auth method at mount "approle"; token accessor="hmac-...", policies=[default victoria-logs], lease=1h0m0s
vaulttls: issued certificate from https://vault.example.com:8200 for CN="victorialogs.example.com", serial 3f:1a:..., expires 2026-07-09T12:00:00Z (in 24h0m0s)
Vault PKI TLS provider ready; certificate expires at 2026-07-09T12:00:00Z
started server at https://0.0.0.0:9428/
```

Логируется `accessor`, а не сам токен: этого достаточно, чтобы найти все действия сервиса в audit log Vault, и нечего утащить из лога. `serial` в той же строке связывает выпуск с записью в `pki/certs`.

Через 16 ч фоновая горутина выпишет новый сертификат без перезапуска (при необходимости заново залогинившись):

```
vaulttls: issued certificate from https://vault.example.com:8200 for CN="victorialogs.example.com", serial 5c:2b:..., expires 2026-07-10T04:00:00Z (in 24h0m0s)
```

### Методы аутентификации

`-tls.vaultAuthMethod` выбирает, как VictoriaLogs получает Vault-токен.

| Метод | Когда использовать | Что лежит на диске |
|-------|--------------------|--------------------|
| `kubernetes` | **Предпочтительный** способ в k8s | Ничего своего: projected-токен service account, который ротирует kubelet |
| `approle` | Вне Kubernetes | `role_id` + `secret_id` (или одноразовый wrapping-токен) |
| `token` | Отладка, совместимость | Статический токен, который кто-то должен доставлять и ротировать |

Токен кэшируется в памяти и перевыпускается, когда израсходовано 2/3 его лизы. Отдельной горутины продления нет: токен нужен только в момент выпуска сертификата (≈ раз в `TTL·2/3`), а повторный логин надёжнее `renew-self`, который упирается в `token_max_ttl`. При остановке процесса токен отзывается через `auth/token/revoke-self`.

**Kubernetes.** Секрета, который можно украсть надолго, здесь нет вовсе — JWT живёт минуты, привязан к audience и проверяется Vault'ом через TokenReview API.

```bash
vault auth enable kubernetes
vault write auth/kubernetes/config \
    kubernetes_host="https://kubernetes.default.svc"

vault write auth/kubernetes/role/victoria-logs \
    bound_service_account_names=victoria-logs \
    bound_service_account_namespaces=logging \
    audience=vault \
    policies=victoria-logs \
    ttl=1h
```

```yaml
# Projected-том вместо legacy-секрета SA: короткий срок жизни и явный audience.
volumes:
  - name: vault-token
    projected:
      sources:
        - serviceAccountToken:
            path: token
            audience: vault
            expirationSeconds: 600
```

```bash
victoria-logs \
  -tls.vaultAddr=https://vault.logging.svc:8200 \
  -tls.vaultCAFile=/etc/vault-tls/ca.crt \
  -tls.vaultAuthMethod=kubernetes \
  -tls.vaultAuthRole=victoria-logs \
  -tls.vaultAuthJWTFile=/var/run/secrets/vault-token/token \
  ...
```

`-tls.vaultAuthRole` — это роль **метода аутентификации**; PKI-роль задаётся отдельным флагом `-tls.vaultRole`. Файл JWT перечитывается при каждом логине, поэтому ротация projected-токена подхватывается сама.

**AppRole.**

```bash
vault auth enable approle
vault write auth/approle/role/victoria-logs \
    token_policies="victoria-logs" \
    token_type=service \
    token_ttl=1h \
    token_max_ttl=4h \
    secret_id_ttl=24h \
    secret_id_bound_cidrs="10.0.0.0/8" \
    token_bound_cidrs="10.0.0.0/8"
```

`secret_id_bound_cidrs`/`token_bound_cidrs` превращают украденный credential в бесполезный за пределами вашей сети, а короткий `secret_id_ttl` ограничивает окно, если credential всё же утёк.

Штатный способ доставки secret_id — **response wrapping**: оркестратор (Vault Agent, init-контейнер, CI) кладёт одноразовый wrapping-токен, а сам secret_id не появляется на файловой системе никогда:

```bash
vault write -wrap-ttl=120s -f auth/approle/role/victoria-logs/secret-id \
    | grep -m1 '^wrapping_token:' | awk '{print $2}' > /run/secrets/vault-secret-id-wrapped
```

```bash
victoria-logs \
  -tls.vaultAuthMethod=approle \
  -tls.vaultAuthRoleIDFile=/run/secrets/vault-role-id \
  -tls.vaultAuthSecretIDWrappedFile=/run/secrets/vault-secret-id-wrapped \
  ...
```

Wrapping-токен одноразовый, поэтому:

- если unwrap не удался — токен либо просрочен, либо **его уже кто-то распаковал**; это и есть механизм обнаружения перехвата. Процесс падает на старте с явным указанием, что делать;
- при каждом старте нужен свежий wrapped-токен. Это операционное требование: без переподкладывания процесс не поднимется после перезапуска.

Распакованный secret_id хранится только в памяти и переиспользуется при повторных логинах.

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
| `-tls.vaultPKIPath` | `pki` | Mount path PKI-движка в Vault. |
| `-tls.vaultRole` | `""` | Имя **PKI**-роли для выпуска сертификатов. |
| `-tls.vaultCommonName` | `""` | CN сертификата. |
| `-tls.vaultAltNames` | `""` | Дополнительные SAN через запятую (DNS и IP). |
| `-tls.vaultTTL` | `24h` | Запрашиваемый TTL. Vault применит `max_ttl` из роли если запрошено больше. |
| `-tls.vaultRenewBefore` | `0` | Насколько до истечения обновлять. По умолчанию `TTL/3`. |
| `-tls.vaultRevokeOnShutdown` | `false` | Отзывать выпущенный сертификат через `pki/revoke` при остановке процесса. Требует `update` на `<pki>/revoke`. |

Аутентификация:

| Флаг | По умолчанию | Описание |
|------|-------------|----------|
| `-tls.vaultAuthMethod` | `token` | `token`, `approle` или `kubernetes`. |
| `-tls.vaultAuthMount` | = метод | Mount path метода аутентификации. |
| `-tls.vaultAuthRole` | `""` | Роль **метода аутентификации** (`kubernetes`). Не путать с `-tls.vaultRole`. |
| `-tls.vaultAuthJWTFile` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | JWT service account. Перечитывается при каждом логине. |
| `-tls.vaultAuthRoleID` / `-tls.vaultAuthRoleIDFile` | `""` | AppRole `role_id`. |
| `-tls.vaultAuthSecretID` / `-tls.vaultAuthSecretIDFile` | `""` | AppRole `secret_id`. Значение маскируется в `/flags`. |
| `-tls.vaultAuthSecretIDWrappedFile` | `""` | Файл с одноразовым wrapping-токеном, из которого распаковывается `secret_id`. |
| `-tls.vaultToken` | `""` | Статический токен (`-tls.vaultAuthMethod=token`). Поддерживает `file:///path` и `https://url` через механизм `Password`. |
| `-tls.vaultTokenFile` | `""` | Путь к файлу с токеном. Перечитывается при каждом обращении — поддерживает ротацию токена. |

Соединение с самим Vault:

| Флаг | По умолчанию | Описание |
|------|-------------|----------|
| `-tls.vaultCAFile` | `""` | PEM-файл с CA, которым доверять дополнительно к системному пулу. |
| `-tls.vaultServerName` | `""` | Имя хоста, проверяемое в сертификате Vault, если оно отличается от `-tls.vaultAddr`. |
| `-tls.vaultInsecureSkipVerify` | `false` | Отключить проверку сертификата Vault. Только для тестовых стендов. |

Исходящие соединения (`-storageNode`, `-remoteWrite.url`) — подробнее в разделе «Исходящий TLS»:

| Флаг | По умолчанию | Описание |
|------|-------------|----------|
| `-tls.vaultClientAuth` | `false` | Предъявлять Vault-сертификат как клиентский (mTLS). Требует `client_flag=true` на PKI-роли; несовместим с `-storageNode.tlsCertFile`/`-remoteWrite.tlsCertFile`. |
| `-tls.vaultTrustPKICA` | `false` | Доверять CA mount'а `-tls.vaultPKIPath` при проверке пира — дополнительно к системному пулу и `*.tlsCAFile`. |

Флаги, относящиеся к чужому методу аутентификации, отвергаются на старте с явным сообщением, а не игнорируются молча.

### Приоритет провайдеров

```
-tls.vaultAddr задан?
    ДА  → HTTP:   сертификат из памяти Vault-провайдера (-tls включается автоматически);
                  явно заданные -tlsCertFile/-tlsKeyFile → фатальная ошибка (конфликт)
          syslog: при -syslog.tls сертификат из памяти того же провайдера;
                  явно заданные -syslog.tlsCertFile/-syslog.tlsKeyFile → фатальная ошибка
    НЕТ → файловые сертификаты (существующее поведение, горячая замена каждые 1 с)
```

syslog подключается к Vault только при явно включённом `-syslog.tls`; без него syslog-слушатель остаётся плейнтекстовым.

### Исходящий TLS: клиентский сертификат и доверие PKI CA

До сих пор речь шла о **входящих** соединениях (сервер отдаёт свой сертификат). Тот же провайдер обслуживает и **исходящие** соединения VictoriaLogs — к storage-узлам (`-storageNode`) и к remote-write назначению vlagent (`-remoteWrite.url`) — через два независимых флага:

| Флаг | По умолчанию | Что делает |
|------|-------------|-----------|
| `-tls.vaultClientAuth` | `false` | Предъявлять Vault-сертификат как **клиентский** на исходящих соединениях (mTLS). PKI-роль должна быть выпущена с `client_flag=true`, иначе пир отвергнет рукопожатие. |
| `-tls.vaultTrustPKICA` | `false` | Проверять пир против CA mount'а `-tls.vaultPKIPath` — **в дополнение** к системному пулу и к `-storageNode.tlsCAFile`/`-remoteWrite.tlsCAFile`, а не вместо них. |

Оба флага реализованы в `lib/vaulttls/client.go` через `vaulttls.NewRoundTripper(ac, tr)` — drop-in-замену `ac.NewRoundTripper(tr)` в `netinsert`, `netselect` и `remotewrite`. Если провайдер не зарегистрирован или оба флага выключены, возвращается обычный `promauth`-round-tripper без изменений, поэтому call-site'ы не содержат условной логики.

Ключевые свойства:

- **CA берётся из самого Vault, без файла.** `-tls.vaultTrustPKICA` читает `GET <pki>/cert/ca_chain` (с откатом на `<pki>/cert/ca`). Этот путь в Vault **не требует токена**, поэтому CA запрашивается без аутентификации. CA перечитывается после каждого успешного продления сертификата и хранится в `atomic.Pointer`, поэтому обновление корневого CA подхватывается без перезапуска.
- **Уже настроенный CA не теряется.** Если для соединения задан `-storageNode.tlsCAFile`/`-remoteWrite.tlsCAFile`, Vault CA добавляется поверх него (пул клонируется), а не заменяет его.
- **Ротация клиентского сертификата прозрачна.** Он отдаётся через колбэк `GetClientCertificate`, поэтому фоновое продление не требует пересоздания транспорта.
- **Взаимоисключение с файлами.** `-tls.vaultClientAuth` нельзя комбинировать с `-storageNode.tlsCertFile`/`-storageNode.tlsKeyFile`/`-remoteWrite.tlsCertFile`/`-remoteWrite.tlsKeyFile` — это фатальная ошибка на старте.

**vlagent** использует обе половины сразу: `vaultflags.Init()` (тот же пакет, что и у victoria-logs) поднимает провайдер, `httpserver` отдаёт из него TLS на `-httpListenAddr`, а `-tls.vaultClientAuth`/`-tls.vaultTrustPKICA` обслуживают исходящий `-remoteWrite.url` — так что клиентский сертификат и CA приходят из Vault, и **ни одного PEM-файла не существует нигде**:

```bash
vlagent \
  -remoteWrite.url=https://victoria-logs:9428/insert/native \
  -tls.vaultClientAuth=true \
  -tls.vaultTrustPKICA=true \
  -tls.vaultAddr=https://vault.example.com:8200 \
  -tls.vaultAuthMethod=approle \
  -tls.vaultAuthRoleIDFile=/run/secrets/vlagent-role-id \
  -tls.vaultAuthSecretIDFile=/run/secrets/vlagent-secret-id \
  -tls.vaultPKIPath=pki \
  -tls.vaultRole=vlagent \
  -tls.vaultCommonName=vlagent
```

PKI-роль для такого клиента отличается от чисто серверной только флагом `client_flag`:

```bash
vault write pki/roles/vlagent \
    allowed_domains="vlagent,localhost" \
    allow_bare_domains=true allow_ip_sans=true \
    client_flag=true server_flag=true \
    max_ttl=72h key_type=ec key_bits=256
```

Когда `-tls.vaultAddr` задан, а `-tls` явно не указан, `-tls` включается автоматически (`https` для `-httpListenAddr`). Если сертификат нужен **только** для исходящих соединений или для syslog, но не для собственного HTTP-сервера, задайте `-tls=false` явно — авто-включение его не перезапишет.

При старте в логах видно загрузку CA (`-tls.vaultTrustPKICA`):

```
vaulttls: loaded the CA of the "pki" pki mount at https://vault.example.com:8200 for verifying outgoing connections
```

**Серверная mTLS-проверка на стороне storage-узла** (проверка клиентского сертификата insert/select-узла) в этот объём не входит: форк `lib/httpserver` пока не пробрасывает `ClientCAs`. Storage-узел проверяет клиента файловым `-tlsCAFile` как и прежде; Vault-провайдер закрывает клиентскую сторону.

### Ротация credential'ов

С `approle` и `kubernetes` токен Vault ротируется сам: он перевыпускается при израсходовании 2/3 лизы, а если Vault отвергнет его раньше (отзыв, рестарт Vault), выпуск сертификата повторяется после свежего логина:

```
vaulttls: vault rejected the cached token with HTTP 403; re-authenticating
```

Ротация того, что лежит на диске:

- **kubernetes** — ничего делать не нужно, projected-токен ротирует kubelet, файл перечитывается при каждом логине;
- **approle** — перезаписать `-tls.vaultAuthSecretIDFile` (подхватится при следующем логине) либо, при response wrapping, положить свежий wrapping-токен и перезапустить процесс;
- **token** — записать новый токен в файл `-tls.vaultTokenFile`: он перечитывается при каждом обращении, перезапуск не нужен.

Если Vault недоступен или credential невалиден, текущий сертификат продолжает раздаваться из памяти до истечения срока, а попытки повторяются каждые 10 с:

```
vaulttls: background renewal failed: ...; will retry in 10s
```

### Защита credentials

**Способ передачи секрета — по убыванию предпочтительности:** файл (`-tls.vaultAuthSecretIDFile`, `file://`) → переменная окружения (`-envflag.enable`) → аргумент командной строки. Последний вариант хуже всех: `/proc/<pid>/cmdline` доступен на чтение всем, а argv попадает в `ps`, `docker inspect` и манифест пода. При инлайновом секрете в argv процесс пишет предупреждение на старте.

**Хранение файлов.** Режим `0600` и владелец — пользователь процесса; при более широких правах в лог пишется предупреждение с именем файла. Файлы лучше держать на tmpfs (в Kubernetes секреты и projected-тома уже tmpfs), а не на постоянном диске.

**Что не попадает в логи.** Токен, `secret_id`, `role_id` и JWT не логируются и не подставляются в тексты ошибок — тело запроса логина в ошибку никогда не включается. Логируются только `accessor` и список политик. Инлайновые значения `-tls.vaultToken` и `-tls.vaultAuthSecretID` маскируются в `/flags` (тип `flagutil.Password` всегда выводит `"secret"`). Это проверяется юнит-тестом `TestProviderDoesNotLeakCredentials`.

**Канал до Vault.** Без `-tls.vaultCAFile` сервер Vault проверяется по системному пулу; подставной Vault просто соберёт отправленные ему credential'ы, поэтому во внутренних PKI указывайте CA явно. `-tls.vaultInsecureSkipVerify` отключает проверку целиком и годится только для стендов. Обращение к Vault по `http://` сопровождается предупреждением: credential и приватный ключ идут открытым текстом.

**Ограничение ущерба.** PKI-роль должна быть узкой: конкретный `allowed_domains`, `client_flag=false` (сертификат только серверный, украденный ключ не подойдёт для клиентской аутентификации), короткий `max_ttl`. Учтите, что `no_store=true` ускоряет выдачу, но делает отзыв невозможным — в том числе `-tls.vaultRevokeOnShutdown`.

**Память процесса.** Приватный ключ и токен живут только в памяти. Занулять их в Go бессмысленно (GC копирует строки), поэтому ставка сделана на короткое время жизни: токен отзывается при остановке, сертификат — при `-tls.vaultRevokeOnShutdown`. На уровне ОС имеет смысл отключить core dump (`ulimit -c 0`), ограничить `ptrace` (`kernel.yama.ptrace_scope`) и не размещать процесс на хосте со swap. `mlockall` намеренно не используется: на лог-БД с многогигабайтным heap он упрётся в `RLIMIT_MEMLOCK`.

**Аудит.** Каждый логин и каждый вызов `POST /v1/pki/issue/<role>` попадает в Vault audit log с accessor'ом токена — тем самым, который печатается в лог при логине.

### Что делать при компрометации

```bash
# 1. Убить утёкший secret_id (accessor виден в `vault list auth/approle/role/victoria-logs/secret-id`)
vault write auth/approle/role/victoria-logs/secret-id-accessor/destroy \
    secret_id_accessor=<accessor>

# 2. Отозвать все токены, выданные этому сервису
vault token revoke -accessor <accessor>

# 3. Отозвать выпущенные сертификаты — они остаются валидными до истечения TTL
vault list pki/certs
vault write pki/revoke serial_number=<serial>

# 4. Найти в audit log всё, что делали скомпрометированным credential'ом
grep <accessor> /vault/logs/audit.log
```

После этого выдать новый `secret_id` (лучше response-wrapped) и перезапустить процесс. Для `kubernetes` шага 1 нет: достаточно снять привязку роли (`bound_service_account_names`) или удалить сам service account.

