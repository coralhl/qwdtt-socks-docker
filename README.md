# qwdtt-socks — SOCKS5-прокси через qWDTT в Docker

Клиент [qwdtt-openwrt](https://github.com/code2fingers/qwdtt-openwrt) (Go), собранный
без тега `openwrt` — этот билд включает userspace WireGuard и SOCKS5-сервер.
SOCKS-режим работает через userspace netstack, поэтому контейнеру не нужны
`NET_ADMIN` и `/dev/net/tun`.

## Файлы

| Файл | Назначение |
| --- | --- |
| `Dockerfile` | Multi-stage сборка: клиент берётся из **релизного тега** GitHub (`ARG QWDTT_VERSION=v1.3.0`) → `scratch` (~18 МБ, статические бинарники) |
| `statusd/` | Мини-супервайзер: запускает клиент, собирает логи, отдаёт страницу статуса |
| `config.json` | Конфигурация подключения (монтируется в контейнер) |
| `docker-compose.yml` | Пример запуска |

Сборка требует доступа к GitHub (скачивается архив тега). Другую версию релиза:

```sh
docker compose build --build-arg QWDTT_VERSION=v1.3.0
```

## Отслеживание релизов и автоматическая сборка

GitHub Actions (`.github/workflows/build.yml`) собирает образ и публикует на GHCR
(`ghcr.io/coralhl/qwdtt-socks`, теги `latest` и `<версия клиента>`) при пушах,
ежедневно по cron, вручную (`workflow_dispatch`) и по событию `repository_dispatch`.
Имя пакета на GHCR не совпадает с именем репозитория (`qwdtt-socks-docker`) — это допустимо,
права и видимость пакета наследуются от репозитория.
При новой версии `code2fingers/qwdtt-openwrt` workflow обновляет `ARG QWDTT_VERSION`
в Dockerfile (коммит), собирает образ с этой версией и уведомляет в ntfy, если в
репозитории заданы секреты `NTFY_URL` и `NTFY_TOKEN`.

Версия клиента и ссылка на исходники показываются на странице статуса
(`/api/status` — поле `client_version`).

## Страница статуса

`statusd` — входная точка контейнера: он запускает клиент, читает его вывод и
показывает страницу на порту 8080 (в compose пробросена на хосте как 8081,
если 8080 занят — порт настраивается в `docker-compose.yml` или через
`STATUS_PORT`). Страница обновляется сама; есть JSON-API:

- `/` — HTML-страница: состояние, режим, peer, число активных worker'ов, время
  подключения, последняя ошибка, живой лог;
- `/api/status` — то же в JSON;
- `/api/log` — последние 500 строк лога.

`docker logs` продолжает работать как раньше — statusd печатает вывод клиента
в stdout контейнера. PAUSE/RESUME/STOP в stdin тоже пробрасываются клиенту.

## Уведомления

При падении и подъёме прокси statusd вызывает скрипт `/usr/local/bin/notify`
(в образе — `notify.sh`) с текстом сообщения как аргументом. Скрипт выбирает
первый настроенный бэкенд:

- ntfy: `NTFY_URL` (+ `NTFY_TOKEN`)
- Telegram: `TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID`
- Gotify: `GOTIFY_URL` + `GOTIFY_TOKEN`

Значения берутся из `.env` (файл не коммитится). Чтобы использовать другой сервис,
достаточно добавить блок в `notify.sh` или подменить сам скрипт — statusd к нему не
привязан. `NOTIFY_CMD=none` отключает уведомления; без настроенного бэкенда
контейнер работает как раньше, только без алертов.

## HTTP-прокси

Клиент умеет только `vpn|socks|rawtun`, поэтому HTTP-прокси добавлен в statusd:
он слушает отдельный порт (`HTTP_PROXY_PORT`, в compose — 8082) и проксирует через
SOCKS5-сервер клиента. Работают обычные запросы с абсолютным URI (`http://...`)
и `CONNECT` (туннель для HTTPS); разрешение имён — на стороне SOCKS. Если заданы
`QWDTT_SOCKS_USER`/`QWDTT_SOCKS_PASS`, они используются и здесь. Пустое значение
`HTTP_PROXY_PORT` отключает слой, `SOCKS_UPSTREAM` меняет адрес SOCKS (по умолчанию
`127.0.0.1:1080`).

```sh
curl -s -x http://127.0.0.1:8082 https://api.ipify.org        # через HTTP-прокси
curl -s --socks5-hostname 127.0.0.1:1080 https://api.ipify.org # через SOCKS5
```

Порт прокси виден на странице статуса и в `/api/status` (поле `http_proxy`).

## Сборка и запуск

```sh
docker pull ghcr.io/coralhl/qwdtt-socks:latest
docker compose up -d
```

Локальная сборка нужна только если меняешь код:

```sh
docker compose build
docker compose up -d
```

Проверка (трафик должен выйти с IP сервера qWDTT, а не локального):

```sh
curl -s https://api.ipify.org                          # напрямую
curl -s --socks5-hostname 127.0.0.1:1080 https://api.ipify.org   # через прокси
```

## Конфигурация (`config.json`)

Поля соответствуют JSON-конфигу клиента (см. `client/client_config.go`):

| Поле | Обязательное | Описание |
| --- | --- | --- |
| `peer` | да | Адрес сервера qWDTT: `host:port` (RAW-слушатель, обычно 56003; для WireGuard-сервера — DTLS-порт, обычно 56000) |
| `hashes` | да | Список хешей звонков VK |
| `password` | да | Пароль подключения (можно через `QWDTT_PASSWORD` в окружении) |
| `dns` | — | DNS для VK. **Важно:** дефолтный `yandex` (UDP 53) часто не работает в контейнере — используйте `doh-google` или `doh-cloudflare` |
| `device_id` | — | ID устройства. **Важно:** сервер привязывает пароль к device ID при первом подключении; ID должен всегда совпадать. Без config-файла клиент использует `unknown`, с config-файлом по умолчанию — `openwrt`. Привязку можно сбросить на сервере |
| `workers` | — | Число сессий (по умолчанию 9) |
| `obfs`, `captcha_mode`, `vk_auth`, `vk_anon_path`, `turn_tcp`, `no_dtls` | — | Необязательные, см. `--help` клиента |

Параметры можно взять из ссылки `wdtt://` или `qwdtt://`:

```sh
echo 'qwdtt://?peer=host:56000&pass=secret&hashes=hash1,hash2' | docker compose run --rm qwdtt-socks -parse-link
```

(Формат `wdtt://connect?v=1&host=...` клиент не разбирает — параметры из такой ссылки переносятся в `config.json` вручную.)

## Часовой пояс

В образе есть tzdata, время в логах и на странице статуса берётся из переменной `TZ`:

```yaml
    environment:
      TZ: Europe/Moscow
```

## Настройки через переменные окружения

Если файл `config.json` не указан или отсутствует, `statusd` формирует конфиг из переменных окружения
(`QWDTT_PEER`, `QWDTT_HASHES` — через запятую, `QWDTT_PASSWORD`, `QWDTT_DEVICE_ID`, `QWDTT_DNS`, `QWDTT_WORKERS`).
Существующий config-файл всегда имеет приоритет. Без `QWDTT_DEVICE_ID` используется `unknown` —
та же привязка, что и в режиме флагов (в config-файле по умолчанию был бы `openwrt`).

Пример запуска без config-файла:

```sh
docker run -d --name qwdtt-socks -p 1080:1080 -p 8081:8080 \
  -e TZ=Europe/Moscow \
  -e QWDTT_PEER=host:56000 \
  -e QWDTT_HASHES='hash1,hash2' \
  -e QWDTT_PASSWORD=secret \
  -e QWDTT_DNS=doh-google \
  qwdtt-socks -mode socks -socks 0.0.0.0:1080
```

## Авторизация SOCKS5

Если заданы `QWDTT_SOCKS_USER` и `QWDTT_SOCKS_PASS`, statusd включает клиентский флаг
`-socks-auth` (SOCKS5 username/password, RFC 1961). Без авторизации соединения отклоняются.

Использование:

```sh
curl --socks5-hostname 127.0.0.1:1080 --proxy-user proxyuser:secretpass https://api.ipify.org
```

В браузере — SOCKS5 с логином/паролем (в Firefox задаётся в настройках прокси; в Chrome
расширения типа SwitchyOmega). Без пароля запрос не пройдёт.

## Тестирование без сервера

```sh
# разбор ссылки
echo 'qwdtt://?peer=host:56000&pass=secret&hashes=hash' | docker compose run --rm qwdtt-socks -parse-link

# проверка живости хешей (нужен доступ к VK)
docker compose run --rm qwdtt-socks -check-hashes -go-dns doh-google
```

## Известные нюансы

- Привязка пароля к device ID — см. выше, это самая частая причина `FATAL_AUTH`.
- VK API недоступен без рабочего DNS из контейнера — фиксируйте `dns: "doh-google"`.
- SOCKS5 слушает `0.0.0.0:1080` внутри контейнера; наружу публикуется через `ports`.
- Управление без перезапуска: в контейнер можно писать в stdin `PAUSE` / `RESUME` / `STOP`.
