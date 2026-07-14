# Обёртка над Makefile для air-gapped CI (GNU make читает GNUmakefile раньше
# Makefile, в том числе во всех рекурсивных вызовах $(MAKE) — в отличие от -f).
#
# Без AIRGAP=1 поведение идентично Makefile.
#
# С AIRGAP=1 цели app-via-docker* собирают *-prod бинарники локальным
# тулчейном Go вместо вложенного `docker run` c образом local/builder:
# в герметичном CI-контейнере (--network=none) нет ни docker, ни apt для
# сборки builder-образа. Флаги компиляции повторяют deployment/docker/Makefile.
# Требования к окружению: кросс-gcc для CGO-платформ и zip для windows-архивов
# (см. deployment/jenkins/builder/Dockerfile).
#
# Использование: AIRGAP=1 make release

include Makefile

ifeq ($(AIRGAP),1)
export AIRGAP

# builder-образ не нужен — сборка идёт текущим тулчейном
package-builder:
	@true

# GOOS/GOARCH/CGO_ENABLED приходят через окружение от вызывающих целей
# (app-via-docker-<goos>-<goarch>); EXTRA_DOCKER_ENVS (CC=..., GOARM=...)
# упстрим передаёт только как --env в docker run, поэтому разворачиваем сами.
app-via-docker:
	$(foreach v,$(EXTRA_DOCKER_ENVS),$(v)) go build -trimpath -buildvcs=false \
		-ldflags "-extldflags '-static' $(GO_BUILDINFO)" \
		-tags 'netgo osusergo' \
		-o bin/$(APP_NAME)$(APP_SUFFIX)-prod $(PKG_PREFIX)/app/$(APP_NAME)

# у windows-цели GOOS/GOARCH в окружение не экспортируются (только GOARCH
# от release-victoria-logs-windows-goarch), задаём явно
app-via-docker-windows:
	CGO_ENABLED=0 GOOS=windows go build -trimpath -buildvcs=false \
		-ldflags "-s -w -extldflags '-static' $(GO_BUILDINFO)" \
		-tags 'netgo osusergo' \
		-o bin/$(APP_NAME)-windows$(APP_SUFFIX)-prod.exe $(PKG_PREFIX)/app/$(APP_NAME)
endif
