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

**Consulta online**, sin instalar nada: **https://jmendozaf.github.io/rucpy/**. La página está en GitHub Pages ([`web/`](web/index.html)) y consulta la API pública en `https://ruc.jmf.dev`.

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
sudo mv ruc-darwin-arm64 /usr/local/bin/ruc
ruc sync
```

### Servidor sin Docker

En [`deploy/`](deploy/) hay una unidad de systemd y una configuración de nginx con HTTPS y límite de consultas por IP:

```sh
curl -L https://github.com/jmendozaf/rucpy/releases/latest/download/ruc-linux-amd64.tar.gz | tar xz
sudo install -m 755 ruc-linux-amd64 /usr/local/bin/ruc
sudo cp deploy/rucpy.service /etc/systemd/system/
sudo systemctl enable --now rucpy     # API en 127.0.0.1:8081, base en /var/lib/rucpy
```

Para que una página en otro dominio pueda consultar la API desde el navegador, listá ese sitio en `RUC_CORS_ORIGINS` (o `--cors`), separado por comas; `*` permite cualquiera.

### Desde el código

```sh
git clone https://github.com/jmendozaf/rucpy && cd rucpy
docker compose up -d
```

## Primeros pasos

Con el binario instalado:

**1. Validar un RUC, sin conexión**

```sh
ruc check 2038893-4    # válido: 2038893-4
ruc check 2038893-5    # inválido: el dígito verificador de 2038893 es 4
```

**2. Descargar el padrón** (~25 segundos, crea una base de ~250 MB)

```sh
export RUC_DB=~/.local/share/ruc.db   # agregalo a tu ~/.zshrc o ~/.bashrc para dejarlo fijo
mkdir -p ~/.local/share
ruc sync
```

Sin `RUC_DB`, la base se crea como `ruc.db` en la carpeta donde estés.

**3. Consultar**

```sh
ruc get 2038893-4                      # por RUC
ruc get MEFA8203705                    # por el código viejo de la SET
ruc search "mendoza franco" --limit 5  # por nombre, sin importar tildes
ruc stats                              # totales por estado y fecha del padrón
```

**4. Levantar la API**

```sh
ruc serve --sync-every 24h
```

En otra terminal (o en el navegador):

```sh
curl localhost:8080/v1/ruc/2038893-4
curl "localhost:8080/v1/search?q=mendoza%20franco"
```

**5. Mantenerlo al día**

```sh
ruc sync    # al día: 2020504 RUC, la DNIT no publicó cambios
```

Solo descarga los archivos que la DNIT haya vuelto a publicar. Con `ruc serve --sync-every 24h` (o la imagen Docker) se hace solo.

## API

| Método | Ruta | Respuesta |
|---|---|---|
| `GET` | `/v1/ruc/{ruc}` | Contribuyente. Acepta `2038893-4`, `2.038.893-4`, `2038893` o el código viejo de la SET (`MEFA8203705`). `404` si no existe, `422` con el dígito correcto si el DV no coincide. |
| `GET` | `/v1/search?q=nombre&limit=20` | Búsqueda por nombre o razón social (mínimo 3 caracteres). Los activos primero. |
| `GET` | `/v1/changes?since=2026-10-01&status=CANCELADO` | Cambios de estado detectados desde esa fecha (por defecto, los últimos 7 días). |
| `GET` | `/v1/stats` | Total, cantidad por estado, fecha de sincronización y versión de cada archivo. |
| `GET` | `/healthz` | `{"status":"ok"}` |

Errores siempre como `{"error": "...", "message": "..."}`. Por defecto la API no responde a páginas de otros dominios; se habilitan con `RUC_CORS_ORIGINS`.

## Línea de comandos

```sh
ruc sync                            # descarga y actualiza ./ruc.db (o $RUC_DB)
ruc get 2038893-4                   # busca un RUC
ruc check 2038893-4                 # valida el dígito verificador, sin conexión
ruc search "mendoza franco" --limit 5  # busca por nombre
ruc changes --since 2026-10-01 --status CANCELADO
ruc stats
ruc serve --addr :8080 --sync-every 24h --cors https://mi-sitio.com
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
