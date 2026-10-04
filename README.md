# rucpy 🇵🇾

Padrón de RUC de Paraguay, local y rápido. Descarga el listado oficial de la DNIT, lo indexa en SQLite y lo expone por línea de comandos o API HTTP.

```sh
$ ruc get 2038893-4
{
  "ruc": "2038893",
  "dv": 4,
  "formatted": "2038893-4",
  "name": "MENDOZA FRANCO, ANIBAL JAVIER",
  "old_code": "MEFA8203705",
  "status": "ACTIVO"
}
```

- **2.020.504 contribuyentes** sincronizados en ~25 segundos (10 archivos en paralelo).
- **Consulta por RUC en ~2 ms**, búsqueda por nombre en ~130 ms, sin importar tildes ni mayúsculas.
- **Valida el dígito verificador** sin conexión (módulo 11 de la DNIT, verificado contra el padrón completo).
- **Historial de cambios de estado** entre sincronizaciones: quién pasó de ACTIVO a SUSPENDIDO o CANCELADO.
- **Un solo binario** sin dependencias, o una imagen Docker de ~14 MB.

## Instalación

### Docker

```sh
docker run -d --name rucpy -p 8080:8080 -v ruc-data:/data ghcr.io/jmendozaf/rucpy
```

La primera vez descarga el padrón completo (unos 25 segundos) y después lo actualiza cada 24 horas. Mientras se actualiza, la API sigue respondiendo con la versión anterior y cambia a la nueva sin cortes. La imagen pesa ~14 MB y funciona en amd64 y arm64.

```sh
curl localhost:8080/v1/ruc/2038893-4
```

Para cambiar la frecuencia, pasá `-e RUC_SYNC_EVERY=12h` (o `0` para no actualizar nunca).

### Binario

Descargá el de tu sistema desde [Releases](https://github.com/jmendozaf/rucpy/releases/latest) (Linux, macOS, Windows):

```sh
curl -L https://github.com/jmendozaf/rucpy/releases/latest/download/ruc-darwin-arm64.tar.gz | tar xz
mv ruc-darwin-arm64 /usr/local/bin/ruc
ruc sync
```

### Desde el código

```sh
git clone https://github.com/jmendozaf/rucpy && cd rucpy
docker compose up -d
```

## API

| Método | Ruta | Respuesta |
|---|---|---|
| `GET` | `/v1/ruc/{ruc}` | Contribuyente. Acepta `2038893-4`, `2.038.893-4`, `2038893` o el código viejo de la SET (`MEFA8203705`). `404` si no existe, `422` con el dígito correcto si el DV no coincide. |
| `GET` | `/v1/search?q=nombre&limit=20` | Búsqueda por nombre o razón social (mínimo 3 caracteres). Los activos primero. |
| `GET` | `/v1/changes?since=2026-10-01&status=CANCELADO` | Cambios de estado detectados desde esa fecha (por defecto, los últimos 7 días). |
| `GET` | `/v1/stats` | Total, cantidad por estado, fecha de sincronización y versión de cada archivo. |
| `GET` | `/healthz` | `{"status":"ok"}` |

Errores siempre como `{"error": "...", "message": "..."}`.

## Línea de comandos

```sh
ruc sync                            # descarga y actualiza ./ruc.db (o $RUC_DB)
ruc get 2038893-4                   # busca un RUC
ruc check 2038893-4                 # valida el dígito verificador, sin conexión
ruc search "mendoza franco" --limit 5  # busca por nombre
ruc changes --since 2026-10-01 --status CANCELADO
ruc stats
ruc serve --addr :8080 --sync-every 24h
```

Códigos de salida: `0` ok, `1` RUC inválido o error, `3` no está en el padrón.

Con Docker sin instalar nada:

```sh
docker run --rm -v ruc-data:/data ghcr.io/jmendozaf/rucpy sync
docker run --rm -v ruc-data:/data ghcr.io/jmendozaf/rucpy get 2038893-4
```

## Cómo funciona

1. Lee la [página del listado de RUC](https://www.dnit.gov.py/web/portal-institucional/listado-de-ruc-con-sus-equivalencias) y encuentra los enlaces `ruc0.zip` … `ruc9.zip` (no están fijos: la DNIT los cambia en cada publicación).
2. Compara la versión de cada archivo con la última sincronización. Si la DNIT no publicó nada nuevo, no descarga ni toca la base.
3. Descarga solo los archivos que cambiaron, en paralelo, y los lee en memoria (sin extraer al disco).
4. Arma una base nueva en un archivo temporal, copiando de la anterior los archivos que no cambiaron, y crea el índice de búsqueda (SQLite FTS5).
5. Compara contra la base anterior y guarda los cambios de estado.
6. Reemplaza la base de forma atómica. Si algo falla (DNIT caída, archivo vacío), la base anterior queda intacta.

El resultado es un único archivo `ruc.db` (~250 MB) que cualquier lenguaje con SQLite puede leer directamente.

### Formato del padrón

Cada archivo trae líneas `RUC|NOMBRE|DV|CÓDIGO VIEJO|ESTADO|` en UTF-8. Algunos RUC terminan en letra (`1023860A`); para el dígito verificador la letra se reemplaza por su código ASCII. En el padrón del 01/10/2026, 2 de los 2.020.504 registros traen un DV que no coincide con el algoritmo (`80045405`, `80044639`, ambos cancelados); `ruc get` devuelve el DV publicado por la DNIT.

## Desarrollo

No hace falta tener Go instalado: todo corre en Docker.

```sh
make test       # tests
make image      # imagen Docker
make binaries   # binarios para Linux, macOS y Windows en ./dist
```

Con Go 1.27 local: `go test ./...` y `go build ./cmd/ruc`.

## Licencia

MIT © Javier Mendoza. Los datos son publicados por la Dirección Nacional de Ingresos Tributarios (DNIT) de Paraguay.
