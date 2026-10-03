# Деплой

## systemd (рекомендуется)

Одна команда с твоего Мака, из корня репозитория:

```bash
deploy/deploy.sh root@IP_СЕРВЕРА
```

Скрипт сам поставит Go и зависимости (если их нет), скопирует исходники,
соберёт бота на сервере, поставит systemd-сервис и запустит. Повторный
запуск = обновление. Локальный `.env` копируется только при первом деплое.

Перед первым деплоем: **отзови старый Telegram-токен** у @BotFather и впиши
новый в `.env` — он будет жить на сервере 24/7.

Полезное на сервере:

```bash
journalctl -u langekko -f          # живые логи
systemctl restart langekko          # перезапуск
ls /opt/langekko/backups            # ежедневные снапшоты базы
```

## Docker (альтернатива)

```bash
docker build -t langekko .
docker run -d --name langekko --restart unless-stopped \
  --env-file .env -e SQLITE_PATH=./data/languagebot.db \
  -v langekko-data:/app/data -v langekko-backups:/app/backups langekko
```
