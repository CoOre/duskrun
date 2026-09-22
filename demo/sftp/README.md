# demo/sftp

Каталог для публичных ключей demo-сервера SFTP (`docker compose --profile demo`).

```sh
ssh-keygen -t ed25519 -N '' -f ./demo/sftp/keys/duskrun     # даст duskrun и duskrun.pub
rm ./demo/sftp/keys/duskrun                                  # приватный ключ здесь не нужен
docker compose --profile demo up -d sftp
```

Приватный ключ регистрируется в duskrun как секрет типа `ssh-key`, а хранилище
создаётся с `host=sftp` (или `127.0.0.1`, порт `DEMO_SFTP_PORT`), `user=backup`,
`path=/backup`.

Сюда попадают только `*.pub`; сам каталог смонтирован в контейнер только на
чтение.
