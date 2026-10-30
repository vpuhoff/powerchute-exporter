# PowerChute Prometheus Exporter (Go)

Экспортер Energy Reporting и Data Log для PowerChute Serial Shutdown. Работает через HTTP с cookie jar, без браузера. Проверен протокол Agent **1.4.0.301 / Back-UPS BX1600MI**. Зависимости закреплены в `go.mod` и `go.sum`; требуется **Go 1.25+**.

## Запуск в Windows

```powershell
go build -trimpath -o bin/powerchute-exporter.exe ./cmd/powerchute-exporter

./bin/powerchute-exporter.exe -config config.local.yaml
```

Для этой установки подготовлены `config.local.yaml` и публичный сертификат `configs/powerchute-local-ca.pem`: TLS проверяется, адрес — `https://localhost:6547`, timezone — `Asia/Yerevan`, валюта — `RUB`. В локальном конфиге `page_size: 100`: полный обход журнала примерно из 2 000 строк укладывается в 30-секундный цикл. По запросу владельца логин и пароль записаны в `powerchute.username` и `powerchute.password` локального конфига; переменные окружения для запуска не нужны. При замене сертификата CA-файл нужно обновить из доверенного источника. `config.local.yaml` исключён из Git.

**На исследованной установке разрешён только один сеанс пользователя.** Перед запуском выйдите из веб-интерфейса кнопкой Log Off. Пока экспортер работает, вход тем же пользователем через браузер может отклоняться. При штатном завершении (Ctrl+C/SIGTERM) экспортер освобождает свой сеанс. После принудительного завершения может понадобиться серверный тайм-аут сеанса.

Проверка:

```powershell
./bin/powerchute-exporter.exe -config config.local.yaml -check-config
Invoke-RestMethod http://127.0.0.1:9787/health
(Invoke-WebRequest http://127.0.0.1:9787/metrics).Content
```

`-check-config` проверяет YAML, ограничения и форматы, не подключается к PowerChute и не читает секреты. `-version` показывает версию сборки.

## Установочные пакеты и службы

### Windows x64

Запустите `powerchute-exporter_<version>_windows_amd64_setup.exe` от администратора. Установщик размещает бинарник в `C:\Program Files\PowerChute Exporter`, создаёт нативную службу **PowerChuteExporter** с отложенным автоматическим запуском под **LocalService** и регистрирует источник событий Application → PowerChuteExporter. Обёртки вроде NSSM или WinSW не нужны.

Конфиг находится в `C:\ProgramData\PowerChute Exporter\config.yaml`. После первой установки отредактируйте адрес, timezone, валюту, учётные данные и при необходимости `tls_ca_file`, затем запустите службу:

```powershell
Start-Service PowerChuteExporter
Get-Service PowerChuteExporter
Stop-Service PowerChuteExporter
```

Новая установка оставляет службу остановленной до настройки. Обновление останавливает работающую службу перед заменой бинарника и запускает её снова; уже остановленная служба остаётся остановленной. Конфиг сохраняется при обновлении и удалении приложения. Доступ к каталогу конфигурации получают SYSTEM, администраторы и LocalService. Приватный локальный конфиг из этого проекта и сертификат вашей установки **не включаются** в публичный установщик.

Для ручной регистрации службы доступны команды из повышенного PowerShell:

```powershell
& 'C:\Program Files\PowerChute Exporter\powerchute-exporter.exe' -service install -config 'C:\ProgramData\PowerChute Exporter\config.yaml'
& 'C:\Program Files\PowerChute Exporter\powerchute-exporter.exe' -service start
& 'C:\Program Files\PowerChute Exporter\powerchute-exporter.exe' -service stop
& 'C:\Program Files\PowerChute Exporter\powerchute-exporter.exe' -service uninstall
```

Команда `-service status` возвращает 0 для работающей службы, 3 для остановленной или отсутствующей и 1 при ошибке. Путь к конфигу хранится в регистрации службы; относительные пути внутри YAML по-прежнему считаются от самого YAML-файла. Журналы службы записываются в Windows Event Log, а при запуске из консоли — в stderr. Остановка через SCM отменяет сбор, завершает HTTP-сервер и освобождает сеанс PowerChute.

### Linux: DEB и RPM, amd64 / arm64

Пакеты предназначены для систем с systemd. Используйте пакет своей архитектуры:

```bash
sudo apt install ./powerchute-exporter_<version>_linux_amd64.deb
# либо на RPM-дистрибутиве:
sudo dnf install ./powerchute-exporter_<version>_linux_amd64.rpm

sudoedit /etc/powerchute-exporter/config.yaml
sudo systemctl start powerchute-exporter
sudo systemctl status powerchute-exporter
journalctl -u powerchute-exporter
```

Пакет создаёт системного пользователя `powerchute-exporter`, устанавливает бинарник в `/usr/bin/powerchute-exporter`, unit в `/usr/lib/systemd/system/powerchute-exporter.service` и включает автозапуск. Первая установка не запускает сервис до настройки. При обновлении перезапускается только уже работающий сервис; отключённый администратором автозапуск не включается повторно.

Конфиги `/etc/powerchute-exporter/config.yaml` и `/etc/powerchute-exporter/environment` имеют права `0640 root:powerchute-exporter`, каталог — `0750`. Поля `username` / `password` можно задать непосредственно в YAML; альтернатива — переменные `POWERCHUTE_USERNAME` и `POWERCHUTE_PASSWORD` в файле `environment`. Служба работает без root, пишет журналы в journald, а системные каталоги для неё доступны только для чтения. CA-файл и файлы секретов должны быть доступны сервисному пользователю и находиться вне домашнего каталога: `ProtectHome=true` закрывает доступ к `/home` и `/root`.

DEB использует conffiles, RPM — `%config(noreplace)`: изменённые конфиги не перезаписываются автоматически при обновлении. Удаление пакета останавливает службу и отключает автозапуск; изменённый конфиг сохраняется (RPM может переименовать его в `.rpmsave`). `apt purge` удаляет пакетные конфиги по стандартным правилам Debian. Системная учётная запись сохраняется, чтобы её UID/GID не достались другому пользователю при оставшихся локальных файлах.

## GitHub Actions

Workflow [`.github/workflows/build.yml`](.github/workflows/build.yml) запускается на push, pull request и вручную через **Actions → Build and package → Run workflow**. Он выполняет `go vet`, тесты Windows/Linux, Linux race-тесты и проверки shell-скриптов, затем собирает:

- Windows amd64: установщик `.exe`, устанавливающий службу, и переносимый `.zip`;
- Linux amd64 и arm64: `.deb`, `.rpm` и переносимый `.tar.gz`;
- SHA256-файлы для проверки артефактов каждой платформы.

Результаты доступны в **Artifacts** завершённого workflow. Push тега вида `v1.2.3` задаёт версию пакетов `1.2.3`; остальные сборки получают версию `0.0.0-dev.<run>+<commit>`. Workflow не требует секретов PowerChute, использует `contents: read` и не публикует GitHub Release автоматически. Для запуска репозиторий нужно разместить на GitHub: локальная папка сама по себе Actions не запускает.

Упаковка закреплена на **nFPM 2.48.0** и **Inno Setup 6.7.4**; скачанные инструменты проверяются по SHA256. Windows job на одноразовом runner проверяет установку, запуск/остановку через SCM, обновление работающей службы с сохранением конфига и удаление. Linux job amd64 проверяет установку, запуск от сервисного пользователя, обновление и удаление в одноразовых Debian Bookworm и Rocky Linux 9 контейнерах; unit дополнительно проверяется `systemd-analyze verify`. В контейнерах systemd не запускается как PID 1, поэтому настоящий systemd lifecycle проверяется при развёртывании на целевой системе. Arm64 пакеты собираются кросс-компиляцией.

Исходники упаковки: `packaging/windows/installer.iss`, `packaging/nfpm.yaml`, `packaging/linux/`. Для локальной сборки установщика сначала соберите бинарник в `dist/windows-amd64/powerchute-exporter.exe`, затем выполните `ISCC.exe /DAppVersion=1.2.3 /DNumericVersion=1.2.3.0 packaging/windows/installer.iss`. Для Linux соберите бинарник в `dist/linux-<GOARCH>/powerchute-exporter`, задайте `VERSION`, `GOARCH` и `PACKAGE_MAINTAINER` (формат `Имя <email>`), затем выполните `nfpm package --config packaging/nfpm.yaml --packager deb --target dist/` и аналогичную команду с `--packager rpm`. В CI maintainer формируется из публичного владельца GitHub-репозитория и его адреса `@users.noreply.github.com`; личные данные не фиксируются в файле упаковки.

## Конфигурация

Полный пример — [`configs/config.example.yaml`](configs/config.example.yaml). YAML строгий: неизвестные поля и несколько YAML-документов отклоняются. Относительные пути считаются от каталога YAML-файла. Все интервалы — в секундах.

Логин и пароль можно задать непосредственно в `powerchute.username` / `powerchute.password`. Для каждого значения приоритет такой: непустое поле YAML → `username_file` / `password_file` → переменная `username_env` / `password_env` (по умолчанию `POWERCHUTE_USERNAME` / `POWERCHUTE_PASSWORD`). Содержимое файла секрета используется без преобразований, кроме удаления одного завершающего CRLF/LF. Значения из YAML сохраняются дословно. Пароли, cookie и токены не выводятся в журналы, метрики или ошибки.

TLS проверяется по умолчанию. Для собственного сертификата задайте `tls_ca_file`; он добавляется к системным CA. `tls_verify: false` доступно только как явная настройка и сопровождается предупреждением в журнале. Соединения не используют HTTP-прокси из окружения. Редиректы ограничены числом, тем же origin и известными страницами. Разрешён безопасный Jetty-суффикс `;jsessionid=...` в действии формы входа.

### Energy Reporting

`period_mode: existing` читает фактически выбранный период без отправки формы. Даты возвращаются в `/health` как ISO-даты, без предположений о включительности границ. Значения выбираются по реальным CSS-селекторам полей, которые можно переопределить в конфигурации.

Числа учитывают явно заданные `decimal_separator` / `thousands_separator`; поддерживаются обычные, неразрывные и узкие неразрывные пробелы. Смешанная или некорректная группировка отклоняется. Валюта задаётся явно, её символ не используется для определения кода.

Режимы `fixed_start_to_today` и `fixed_range` реализованы, но требуют явного `allow_period_update: true` и проверенного `post_date_format`, например `%m/%d/%Y`. Начало и конец в конфигурации — ISO `YYYY-MM-DD`. Перед POST читаются свежие токены; после него проверяются фактически принятые даты. При совпадении дат POST не выполняется. В режиме до сегодняшнего дня используется timezone PowerChute. POST изменения периода не повторяется автоматически после ошибки авторизации.

**На реальной установке изменение периода не выполнялось.** Формат POST, необходимость `apply`, включительность дат и область сохранения периода требуют отдельной проверки. Выбор периода может повлиять на веб-интерфейс другого сеанса/пользователя.

### Data Log

Реальный ответ содержит `aaData` с объектами (`date`, `time`, `linevolt`, `battvolt`, `battcap`, `upsload`), `iTotalDisplayRecords`, `iTotalRecords` и `sEcho`. Адаптер использует JSON pointers `rows_path` и `total_path`. Для другого контейнера их можно изменить; массивы строк вместо объектов требуют другого адаптера. Пустой `total_path` допускает API без счётчика строк.

По умолчанию `ordering: scan`: ограниченная пагинация **всего** журнала, максимум timestamp по всем страницам, проверка общего количества и повторное чтение первой страницы для обнаружения изменений во время обхода. Одинаковые повторные страницы, изменение total, неполный обход и превышение `max_pages` / 30-секундного цикла приводят к ошибке источника. По умолчанию `page_size: 100`, что подтверждено интерфейсом и реальным API. При размере 10 обход примерно 2 000 строк на исследованной установке не успевает завершиться за 30 секунд. Если журнал большой, настройте размер страницы и лимит цикла в пределах подтверждённых возможностей сервера.

`ordering: newest_first` читает только первую страницу и проверяет порядок timestamp внутри неё. Этот режим допустим **только после независимого подтверждения сортировки сервером**: сама проверка одной страницы не доказывает отсутствие более новой записи на других страницах. В текущей реализации и локальном конфиге используется полный обход.

Для исследованной установки проверены все 2078 строк на 21 странице: порядок от новых к старым, первая строка — глобальный максимум. Смещения и размеры страниц согласованы; переданные `asc` / `desc` порядок не изменили. Результаты, границы проверки и опциональные live-тесты описаны в [`docs/powerchute-api-ordering.md`](docs/powerchute-api-ordering.md). Исследование не меняло режим установленного экспортера.

Дата и время задаются в формате strftime: `%m`, `%d`, `%Y` или `%y`, `%H`, `%M`, `%S`. По умолчанию `%m/%d/%y %H:%M:%S`, timezone задаётся обязательно. Числовые поля месяца/дня/времени принимают одну или две цифры: в реальном журнале встречается `10/9/26`. Порядок полей и ширина года остаются явными. `two_digit_year_start: 2000` задаёт окно **2000..2099**, без неявного правила Go для 1969/2069. Неоднозначные времена при переходе с летнего времени и несуществующие локальные времена отклоняются.

Для поиска последней записи проверяются timestamp всех строк. Предметные значения проверяются у выбранной последней записи: пропуски и нечисловые значения — ошибка; заряд должен быть 0..100%, напряжения и нагрузка неотрицательны, верхний предел напряжения не придумывается. Нагрузка выше 100% сохраняется. Напряжение 0 V не скрывается. Возраст и clock skew проверяются отдельно от успешности получения записи.

## Метрики и состояние

`GET /metrics` читает только память и доступен при ошибках PowerChute. Оба источника собираются в фоне (60 / 300 секунд, положительный jitter до 20%). Все обращения к PowerChute сериализованы. Тайм-аут цикла и `powerchute_scrape_duration_seconds` отсчитываются после получения очереди сбора: ожидание другого источника не расходует собственный бюджет и не создаёт ложную сетевую ошибку. На цикл источника приходится максимум один вход и один повтор безопасного чтения; неверный пароль или занятый сеанс включают общий cooldown не менее 60 секунд. Ошибки дополнительно увеличивают интервал источника до восьми базовых интервалов плюс jitter.

Снимок каждого источника публикуется атомарно. До первого успеха его предметных метрик нет. После ошибки последний успешный снимок сохраняется до TTL, а `powerchute_scrape_success` сразу становится 0. По истечении TTL предметные метрики исчезают. Метрики состояния и счётчики ошибок остаются.

| Метрика | Тип / labels |
| --- | --- |
| `powerchute_energy_kwh` | Gauge, энергия за фактический период |
| `powerchute_energy_cost` | Gauge, `currency` |
| `powerchute_co2_kg` | Gauge, CO₂ за период |
| `powerchute_input_voltage_volts` | Gauge |
| `powerchute_battery_voltage_volts` | Gauge |
| `powerchute_battery_charge_ratio` | Gauge, проценты / 100 |
| `powerchute_load_ratio` | Gauge, проценты / 100 |
| `powerchute_datalog_timestamp_seconds` | Gauge, Unix-время записи |
| `powerchute_datalog_age_seconds` | Gauge, текущий возраст записи |
| `powerchute_datalog_fresh` | Gauge, возраст в пределах `[-max_future_skew_seconds, max_age_seconds]` |
| `powerchute_scrape_success` | Gauge, `source=energy\|datalog` |
| `powerchute_scrape_duration_seconds` | Gauge, `source` |
| `powerchute_last_success_timestamp_seconds` | Gauge, `source`, отсутствует до первого успеха |
| `powerchute_scrape_errors_total` | Counter, `source`, `reason` |
| `powerchute_auth_attempts_total` | Counter, `result=success\|failure`, фактические POST входа |
| `powerchute_exporter_build_info` | Gauge=1, `version` |

`reason` ограничен набором `network`, `tls`, `authentication`, `http`, `parse`, `empty`, `period`, `timestamp`. В журнале дополнительно выводятся безопасное уточнение `detail` и длительность попытки: например, тайм-аут всего цикла или отдельного запроса. Уточнение может содержать только известный путь endpoint без origin, query и Jetty session ID; исходный текст HTTP-исключения не выводится. Labels не содержат даты, URL или тексты исключений. Исторические timestamps у samples не задаются. Итоги Energy Reporting — Gauge; применять `rate()` к ним как к счётчикам нельзя.

`GET /health` возвращает JSON и HTTP 200, когда у всех включённых источников есть снимок в пределах TTL и Data Log свежий; иначе 503. Последняя HTTP-ошибка сама по себе не делает пригодный кеш непригодным. В JSON видны результат последней попытки, время последнего успеха, фактический период и свежесть журнала. Отключённые источники не участвуют в проверке.

Минимальный Prometheus-конфиг:

```yaml
scrape_configs:
  - job_name: powerchute
    scrape_interval: 30s
    static_configs:
      - targets: ['127.0.0.1:9787']
```

Loopback доступен только локальному Prometheus. Для удалённого сбора явно измените `listen_address` и настройте доступ в вашей сети.

## Grafana

Готовый дашборд для импорта: [`grafana/powerchute-exporter.json`](grafana/powerchute-exporter.json). В Grafana откройте **Dashboards → New → Import**, загрузите JSON и выберите в верхних фильтрах источник **Prometheus**, **Job**, **UPS / instance** и валюту. UID data source в файле менять не требуется.

Дашборд содержит 32 панели: напряжения, заряд, нагрузку, свежесть Data Log, итоги Energy Reporting и диагностику экспортера. Отсутствующие значения не подменяются нулями; итоги отчёта не превращаются в счётчики. Описание и особенности интерпретации — в [`grafana/README.md`](grafana/README.md).

## Сборка и проверки

```powershell
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -ldflags '-s -w -X main.version=0.1.0' -o bin/powerchute-exporter.exe ./cmd/powerchute-exporter
```

На Windows для `-race` нужен совместимый GCC/MinGW в PATH. Обычные тесты используют локальные `httptest`-серверы и обезличенные фикстуры реальной установки в `internal/powerchute/testdata`; подключения к реальному агенту не выполняются.

Для установленного здесь MinGW 9.2 race-тесты проходят с внешней линковкой библиотеки синхронизации:

```powershell
go test -race '-ldflags=-linkmode=external -extldflags=-lsynchronization' ./...
```

Проверка реального агента включается отдельно. Она создаёт сеанс, читает оба источника, завершает свой сеанс и повторно входит; режим периода должен быть `existing`. В конце тест освобождает сеанс. Убедитесь, что веб-интерфейс вышел из системы.

```powershell
$env:POWERCHUTE_LIVE_TEST = '1'
$env:POWERCHUTE_CONFIG = (Resolve-Path config.local.yaml).Path
go test ./internal/powerchute -run TestLiveSessionReconnection -v -count=1
go test ./internal/exporter -run TestLiveBackgroundHTTP -v -count=1
Remove-Item Env:POWERCHUTE_LIVE_TEST
```

Сборка контейнера: `docker build --build-arg VERSION=0.1.0 -t powerchute-exporter .`. В смонтированном конфиге задайте `listen_address: 0.0.0.0` для публикации порта контейнера. `localhost:6547` внутри контейнера указывает на контейнер, поэтому для Windows-хоста обычно нужен достижимый адрес хоста; сертификат должен соответствовать этому имени. Конфиг, CA и секреты монтируются отдельно, образ запускается без root. Docker-сборка требует доступ к официальным базовым образам и Go-модулям.

Экспортер не использует `/ajaxbanner` или `/status` как источники метрик, не меняет настройки ИБП, не очищает журналы и не выполняет диагностику или выключение.

На локальной установке 31 октября 2026 года прошли оба live-теста: два входа с завершением сеанса между ними, чтение обоих источников с проверкой TLS, фоновый сбор, HTTP 200 `/health` и десять одновременных корректных Prometheus scrape. Обычные тесты, `go vet` и race-тесты с указанной внешней линковкой также прошли. Бинарники Windows amd64 и Linux amd64 собраны; контейнер отдельно не запускался. Во время live-проверок использовался только режим периода `existing`, тестовые сеансы завершены.
