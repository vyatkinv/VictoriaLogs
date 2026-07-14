// Jenkinsfile для сборки VictoriaLogs в закрытом (air-gapped) контуре.
//
// Требования к Jenkins:
//   - плагины: workflow-aggregator (Pipeline), git, docker-workflow, ws-cleanup;
//   - Linux-агент с Docker;
//   - образ BUILD_IMAGE доступен во внутреннем registry (зеркало golang:1.26-bookworm,
//     минорная версия Go должна быть >= версии из go.mod).
//
// Доступ в интернет НЕ требуется: все зависимости закоммичены в vendor/,
// поэтому сборка идёт с -mod=vendor и GOPROXY=off. Стадии Build/Test выполняются
// в контейнере с --network=none — это гарантирует герметичность сборки на каждом
// прогоне (любая попытка что-то скачать упадёт сразу, а не в проде).
//
// Если в вашем форке vendor/ не коммитится, замените в environment:
//   GOFLAGS = '-mod=mod'
//   GOPROXY = 'https://artifactory.corp.local/api/go/go-remote'   // go-репозиторий Artifactory (remote proxy)
//   GONOSUMDB/GONOSUMCHECK не нужны — достаточно GOSUMDB=off и GOFLAGS без -mod=vendor
// и уберите --network=none из args (контейнеру нужен доступ к Artifactory).

def BUILD_IMAGE = 'golang:1.26-bookworm' // TODO: замените на внутренний registry, напр. registry.corp.local/mirror/golang:1.26-bookworm

pipeline {
    // checkout выполняется на узле с доступом к корпоративному git
    agent { label 'linux' }

    parameters {
        booleanParam(name: 'RUN_TESTS', defaultValue: true, description: 'Запускать юнит-тесты')
    }

    options {
        disableConcurrentBuilds()
        buildDiscarder(logRotator(numToKeepStr: '20'))
    }

    stages {
        stage('Build & Test (offline)') {
            agent {
                docker {
                    image BUILD_IMAGE
                    // reuseNode: используем workspace верхнего агента (исходники уже там),
                    // --network=none: сборка обязана пройти без единого сетевого вызова
                    reuseNode true
                    args '--network=none'
                }
            }
            options { skipDefaultCheckout true }
            environment {
                // все зависимости берутся из vendor/, любые обращения к proxy запрещены
                GOFLAGS     = '-mod=vendor'
                GOPROXY     = 'off'
                GOSUMDB     = 'off'
                // запрет автоскачивания тулчейна из go.mod: используем Go из образа
                GOTOOLCHAIN = 'local'
                GOCACHE     = "${WORKSPACE}/.gocache"
                // у пользователя контейнера (uid агента) нет домашнего каталога
                HOME        = "${WORKSPACE}"
            }
            stages {
                stage('Env') {
                    steps {
                        sh 'go version && go env GOPROXY GOFLAGS GOTOOLCHAIN'
                    }
                }
                stage('Vet') {
                    steps {
                        sh 'make vet'
                    }
                }
                stage('Test') {
                    when { expression { params.RUN_TESTS } }
                    steps {
                        // -tags synctest обязателен (см. CLAUDE.md): переключает lib/fasttime на реальный time.Now()
                        sh "go test -tags 'synctest' ./lib/... ./app/..."
                    }
                }
                stage('Build') {
                    steps {
                        sh 'make victoria-logs vlagent vlogscli'
                    }
                }
            }
        }
    }

    post {
        success {
            archiveArtifacts artifacts: 'bin/*', fingerprint: true
        }
        cleanup {
            cleanWs(deleteDirs: true, notFailBuild: true)
        }
    }
}
