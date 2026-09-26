# MockVision

Cámaras IP simuladas en una red real. Cada cámara entra a tu LAN con su
propia IP y MAC, sirve RTSP y la API HTTP que describe su perfil de
fabricante, y envía eventos a tu VMS, NVR o backend. Se administran desde un
panel web que sirve el propio nodo.

Sirve para probar software que consume cámaras sin comprarlas y sin tocar
equipos en producción. Simula lo que una cámara hace hacia afuera; no la
reemplaza.

> **Estado: demo técnica.** Un perfil de fabricante, RTSP desde una imagen,
> tres rutas HTTP y un evento manual de cruce de línea, de punta a punta.
> [docs/DEMO-NOTES.md](docs/DEMO-NOTES.md) lista lo simplificado.
> Read in English: [README.md](README.md).

## Qué hace la demo

- Una cámara creada desde el panel aparece en la LAN con su propia IP y MAC
  (macvlan). Antes de tomar la IP hace un sondeo ARP, y se anuncia con ARP
  gratuito.
- Streams RTSP en bucle a partir de una imagen: principal, secundario y
  tercero, como los define el perfil, en H.264, H.265 o MJPEG. Cada imagen
  se codifica una sola vez por configuración de stream y el bucle casi no
  usa CPU.
- Una API HTTP definida en un perfil YAML, con autenticación Digest:
  snapshot, información del equipo y lectura y escritura de un parámetro.
- Un evento de cruce de línea, disparado desde el panel y enviado a un
  destino con un POST HTTP. Cada entrega queda registrada con su estado y su
  latencia.
- Métricas por cámara (CPU, RAM, clientes). Si una cámara superaría el
  máximo de cámaras o los recursos del nodo, se rechaza indicando el motivo.
- Un panel que se actualiza en vivo por WebSocket.

## Requisitos

- Linux amd64 o arm64: Debian 12 o 13, Ubuntu 24.04 o posterior, o
  Raspberry Pi OS de 64 bits. El kernel necesita macvlan.
- **Red cableada.** macvlan le da a cada cámara una MAC extra, y el Wi-Fi no
  acepta MACs extra. Los switches con port security también pueden
  bloquearlas. En una VM, el hipervisor tiene que permitir el modo promiscuo
  o el cambio de MAC.
- Docker con Compose v2, o una instalación nativa con FFmpeg y systemd.
  Docker Desktop en Windows o macOS no funciona, porque corre detrás de un
  NAT.

## Inicio rápido con Docker

```sh
git clone https://github.com/corticoide/MockVision.git
cd MockVision
docker compose up -d   # la primera vez construye la imagen
```

Abre `http://<nodo>:8080`. En la primera visita el panel pide crear el
administrador; no hay credenciales por defecto. Después:

1. **Profiles → Import profile:** elige `profiles/milesight-demo.yaml`. Se
   valida y queda listado como *Draft* (borrador).
2. **Targets → New target:** pon la URL que tiene que recibir los eventos,
   por ejemplo `http://192.168.1.10:8000/events`.
3. **Cameras → New camera:** pon una IP libre de tu LAN y elige el destino.
   La cámara arranca y pasa a *Running*.

Desde **otro equipo** de la misma LAN (la IP `192.168.1.50` y la contraseña
`secret` son ejemplos):

```sh
ping 192.168.1.50
ip neigh show 192.168.1.50   # la MAC propia de la cámara, no la del nodo
ffprobe rtsp://admin:secret@192.168.1.50:554/main   # también /sub y /third
curl --digest -u admin:secret -o snapshot.jpg http://192.168.1.50/snapshot.cgi
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/operator.cgi?action=get.system.information"
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/param.cgi?action=set&Image.Brightness=70"
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/param.cgi?action=get&name=Image.Brightness"
```

El usuario de la cámara es el que se cargó al crearla. Si la contraseña quedó
vacía, la cámara usa la cuenta de fábrica del perfil (`admin` / `ms1234`).
Pulsa **Line crossing** en la cámara: el destino recibe el POST, y **Events**
muestra la entrega, el código HTTP y la latencia.

> **Prueba desde otro equipo.** Linux no deja que un equipo alcance sus
> propias interfaces macvlan. Por eso el nodo no puede abrir los streams de
> sus cámaras, y sus cámaras no pueden entregar eventos a un receptor que
> corra en el nodo. Pon el cliente y los destinos en otras máquinas. La
> vista previa del snapshot en el panel funciona igual, porque no usa la red.

### Automatización con tokens de API

Crea un token en **Configuración → Tokens de API**; se muestra una sola vez.
Los scripts y la CI lo envían en la cabecera Bearer, sin cookie ni
encabezado propio:

```sh
TOKEN=mvt_…   # de Configuración
curl -H "Authorization: Bearer $TOKEN" "http://<nodo>:8080/api/v1/cameras?state=running"
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"stop","ids":["<id de cámara>"]}' http://<nodo>:8080/api/v1/cameras/actions/bulk
```

Un token de lectura solo consulta el nodo (y abre el WebSocket); uno de
escritura puede cambiarlo, salvo los tokens, que se administran únicamente
desde el panel. **Auditoría** muestra cada cambio con su origen: el panel, la
API con el nombre del token, un cliente de la API emulada de una cámara o el
propio nodo.

### Trabajos en segundo plano

Codificar un stream e importar un paquete corren como trabajos
(**Trabajos** en el panel, `/api/v1/jobs` en la API), con su progreso y su
historial. Corren a lo sumo dos a la vez (**Configuración → Límites**); el
resto espera en la cola. Cerrar el navegador no detiene nada, y un trabajo
cortado por un reinicio queda *Interrumpido* hasta que lo reanudas desde su
último punto de control. **Preparar variantes** codifica de antemano todos
los streams que necesitan las cámaras, así arrancar muchas no espera nada;
si una falla, pregunta si reintentar, omitirla o detenerse, y la omite si
nadie responde en diez minutos. Un trabajo que espera tu respuesta no
frena a los demás.

### Streams y códecs

Como una cámara real, cada cámara codifica la misma imagen una vez por uso:
el stream **principal** para grabar, uno **secundario** liviano para
mosaicos y celulares, y un **tercero**, a menudo MJPEG, para clientes
simples. Cada uno tiene su dirección RTSP (`rtsp://<ip>/main`, `/sub`,
`/third` con el perfil demo), que la pestaña **Medios** de la cámara muestra
junto a su instantánea. El códec, la resolución, los cuadros por segundo, el
bitrate y el GOP se cambian ahí, o desde la API propia de la cámara, cuando
el perfil les vincula un parámetro; el stream se codifica de nuevo y la
cámara cambia a él sin reiniciarse.

- **H.264** se reproduce en todos los clientes. **H.265** necesita cerca de
  la mitad del bitrate para la misma imagen, pero no todos los clientes lo
  reproducen. **MJPEG** envía cada cuadro como un JPEG; por RTSP admite a lo
  sumo 2040×2040 en múltiplos de 8.
- Un cliente que se conecta recibe un cuadro clave enseguida, como de un
  codificador real.

### Configuración

`compose.yaml` pasa los dos ajustes que necesita la mayoría de las
instalaciones. El resto va en su sección `environment`.

| Variable | Por defecto | Qué es |
|---|---|---|
| `MOCKVISION_LISTEN` | `:8080` | Dirección del panel y de la API; `<ip>:<puerto>` para escuchar solo en una IP de gestión |
| `MOCKVISION_PARENT_IF` | interfaz de la ruta por defecto | Interfaz a la que se conectan las cámaras; se puede cambiar en Settings del panel |
| `MOCKVISION_DATA` | `/data` | Base de datos, clave del nodo, imágenes y streams codificados |
| `MOCKVISION_FFMPEG` | `ffmpeg` | Binario de FFmpeg |
| `MOCKVISION_SECURE_COOKIES` | apagado | `1` detrás de un proxy inverso HTTPS |
| `MOCKVISION_ALLOWED_ORIGINS` | ninguno | Orígenes extra (`host:puerto`) que pueden llamar a la API |
| `MOCKVISION_LOG_LEVEL`, `MOCKVISION_LOG_FORMAT` | `info`, texto | `debug`…`error`; `json` |
| `MOCKVISION_SERVICE_USER`, `MOCKVISION_CAMERA_USER` | `mockvision`, `mockvision-cam` | Usuarios del servicio principal y de las cámaras |

Los límites (máximo de cámaras, umbrales de RAM y CPU, retención de eventos)
se configuran en **Settings**.

### Sin Docker

Instala FFmpeg, compila (`make build`, requiere Go 1.27 y Node 22) e instala
el binario con la unidad de systemd de
[deploy/mockvision.service](deploy/mockvision.service); su encabezado lista
los pasos.

## Cómo funciona

Un único binario corre como tres tipos de proceso:

```
mockvision run      root, 9 capacidades     helper de red: namespaces, macvlan, ARP, arranque de cámaras
 └─ mockvision serve   uid mockvision, ninguna   panel, API REST, WebSocket, SQLite, reconciliador, FFmpeg
 └─ mockvision camera  uid mockvision-cam, ninguna   una por cámara, dentro de su namespace sim-<nombre>
```

- El **helper de red** es el único proceso con privilegios. Acepta un
  conjunto cerrado de pedidos validados del servicio, por un socket privado.
  Nunca ejecuta una shell.
- El **servicio principal** guarda el estado deseado en SQLite y lo
  reconcilia con el kernel al arrancar y cada 10 segundos. Las cámaras con
  autoarranque vuelven después de un reinicio.
- Cada **cámara** recibe sus sockets ya abiertos en su namespace. Suelta
  todos los privilegios antes de leer cualquier entrada: sin capacidades, con
  un usuario aparte y con `no_new_privs`. Después sirve los motores de su
  perfil (`rtsp`, `http-api`, `http-push`) y habla con el servicio en líneas
  JSON.

Los namespaces con nombre permiten depurar con `ip netns exec sim-<cámara> …`
(con Docker: `docker compose exec mockvision ip netns`). Donde el perfil
AppArmor de Docker prohíbe los montajes, las cámaras usan namespaces
anónimos.

La arquitectura, el formato de perfiles, la API y las decisiones vienen del
documento de diseño del proyecto; `backend/internal/api/openapi.yaml`
describe la API REST.

## Desarrollo

```sh
make dev                     # modo local: sin privilegios, cámaras en 127.0.0.1 con puertos propios
cd frontend && npm run dev   # panel con recarga en caliente en :5173, con proxy al nodo en :8080
```

El modo local sirve para trabajar en el panel, la API y los motores. No
tiene IP ni MAC en la LAN.

| Comando | Qué corre |
|---|---|
| `make test` | `go vet`, tests unitarios y el chequeo de tipos del panel |
| `make test-integration` | namespaces de red y macvlan sobre un enlace virtual (root) |
| `make e2e` | los criterios de aceptación de la demo en una LAN virtual aislada (root, iproute2, ffmpeg, curl, python3) |
| `make e2e-compose` | los mismos criterios contra la imagen Docker levantada con `compose.yaml` |
| `make generate` | consultas de sqlc y tipos de la API del panel desde `openapi.yaml` |

El binario se compila con `CGO_ENABLED=0`: soltar privilegios cambia todos
los hilos a la vez, y eso solo lo puede hacer un binario Go puro.

## Seguridad

- La primera ejecución crea el administrador. Las contraseñas usan Argon2id,
  y los fallos repetidos bloquean el login.
- Las sesiones son cookies `HttpOnly` y `SameSite=Strict`. Todo pedido que
  cambia estado necesita un encabezado propio y pasa un chequeo de `Origin`.
  El panel corre con una CSP estricta y no carga nada de otros orígenes.
- Las contraseñas de cámaras y destinos se cifran con XChaCha20-Poly1305. La
  clave se guarda fuera de la base, y la API nunca las devuelve.
- Los tokens de API se guardan como hash SHA-256 y se muestran una sola vez.
  Tienen alcance (lectura o escritura), pueden vencer y se revocan al
  instante desde el panel. La auditoría guarda cada cambio 90 días con su
  origen y su IP.
- El panel es HTTP plano. Ponlo detrás de un proxy inverso HTTPS
  (`MOCKVISION_SECURE_COOKIES=1`) antes de exponerlo fuera de una red de
  laboratorio.

## Licencia

[Apache 2.0](LICENSE)
