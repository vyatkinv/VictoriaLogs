# AIRGAP_RELEASE_MIGRATION.md

Пошаговая инструкция по миграции air-gapped Jenkins-пайплайна со сборки трёх бинарников (`make victoria-logs vlagent vlogscli` в `golang:1.26-bookworm`) на полную релизную сборку (`make release`: tar.gz/zip + sha256-чексуммы для всех платформ, без публикации). Контекст и общие принципы — в [AIRGAP_CI_GUIDE.md](AIRGAP_CI_GUIDE.md).

---

## Что меняется

Коммит `ce2669535` приносит три файла:

- **`GNUmakefile`** (корень) — оверлей над Makefile: с `AIRGAP=1` цели `app-via-docker*` собирают `*-prod` бинарники локальным тулчейном Go с теми же флагами (`-trimpath`, `-extldflags '-static'`, `-tags 'netgo osusergo'`, `CC=` для CGO-платформ) вместо вложенного `docker run`, недоступного в герметичном CI-контейнере. Без `AIRGAP=1` поведение make не меняется вообще.
- **`deployment/jenkins/builder/Dockerfile`** — builder-образ: golang + `gcc-aarch64-linux-gnu` + `libc6-dev-arm64-cross` (CGO-кросс для linux-arm64) + `zip` (windows-архивы).
- **`Jenkinsfile`** — стадия Build заменена на Release (`make release`), в env добавлен `AIRGAP=1`, в артефакты уходят `bin/*.tar.gz`, `bin/*.zip`, `bin/*_checksums.txt`.

## Шаг 1. Заберите код

Смержите/черри-пикните коммит `ce2669535` в вашу внутреннюю ветку. Конфликтов со старым пайплайном не будет: Jenkinsfile меняется поверх прежнего, остальные файлы новые.

## Шаг 2. Соберите новый builder-образ (во внешнем контуре)

Старого зеркала `golang:1.26-bookworm` теперь недостаточно — релизу нужны кросс-gcc и zip:

```bash
docker build --build-arg go_builder_image=golang:1.26-bookworm \
  -t vl-airgap-builder:1.26-bookworm deployment/jenkins/builder
```

Сборка образа делает `apt-get update` — гоните её там, где есть интернет. Если внутри контура есть apt-зеркало, можно собирать и внутри, подставив базовый образ из внутреннего registry.

## Шаг 3. Перенесите образ в контур

```bash
docker save vl-airgap-builder:1.26-bookworm | zstd > vl-airgap-builder.tar.zst
# внутри контура:
zstd -d < vl-airgap-builder.tar.zst | docker load
docker tag vl-airgap-builder:1.26-bookworm registry.corp.local/mirror/vl-airgap-builder:1.26-bookworm
docker push registry.corp.local/mirror/vl-airgap-builder:1.26-bookworm
```

Либо одной командой `skopeo copy` между registry, если есть маршрут.

## Шаг 4. Поправьте имя образа в Jenkinsfile

В коммите стоит тестовое имя — замените на внутреннее:

```groovy
def BUILD_IMAGE = 'registry.corp.local/mirror/vl-airgap-builder:1.26-bookworm'
```

Больше в Jenkinsfile ничего менять не нужно: `AIRGAP=1` уже в environment стадии, метка агента (`linux`), параметр `RUN_TESTS` и набор плагинов — те же, что раньше.

## Шаг 5. Проверьте ёмкость под артефакты

Раньше архивировались 3 бинарника (~60 МБ), теперь 40 файлов на ~250 МБ за билд. Проверьте квоту artifact storage; `buildDiscarder` уже ограничивает историю 20 билдами (~5 ГБ) — при необходимости уменьшите.

## Шаг 6. Первый прогон и smoke-check

Запустите джобу и убедитесь:

- в консоли релизные цели идут как `CC=... go build ...`, а не `docker run`;
- в артефактах 20 архивов + 20 `_checksums.txt`;
- выборочно: `sha256sum -c <archive>_checksums.txt` и `tar -tzf <archive>.tar.gz`.

---

## Нюансы на будущее

- **Имена артефактов.** `PKG_TAG` берётся из git-тега на HEAD; без тега — из `git describe` (получится `...-heads-master-0-g<sha>.tar.gz`). Для настоящих релизов тегайте коммит — имена станут `victoria-logs-linux-amd64-v1.x.x.tar.gz`.
- **Обновление Go.** Раньше при бампе go.mod достаточно было перезеркалить `golang:X.Y`; теперь это пересборка и перенос `vl-airgap-builder` (шаги 2–3).
- **Откат.** Без `AIRGAP=1` GNUmakefile ничего не меняет, а старый Jenkinsfile из истории ветки продолжит работать с чистым golang-образом.
