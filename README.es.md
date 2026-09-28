# MockVision

Cámaras IP simuladas en una red real. Cada cámara entra a tu LAN con su
propia IP y MAC, sirve RTSP y la API HTTP que describe su perfil de
fabricante, y envía eventos a tu VMS, NVR o backend. Se administran desde un
panel web que sirve el propio nodo.

Sirve para probar software que consume cámaras sin comprarlas y sin tocar
equipos en producción. Simula lo que una cámara hace hacia afuera; no la
reemplaza.

> **Estado: v1 en desarrollo.** Por ahora un perfil de fabricante;
> [docs/DEMO-NOTES.md](docs/DEMO-NOTES.md) lista lo que sigue simplificado.
> Read in English: [README.md](README.md).

## Qué hace la demo

- Una cámara creada desde el panel aparece en la LAN con su propia IP y MAC
  (macvlan), o con la MAC del nodo en Wi-Fi (ipvlan). Toma una IP estática o
  la pide por DHCP, y si no hay servidor usa su dirección de fábrica, como
  una real. Antes de usar su IP y su MAC las sondea en la LAN, y se anuncia
  con ARP gratuito.
- Streams RTSP en bucle a partir de una imagen: principal, secundario y
  tercero, como los define el perfil, en H.264, H.265 o MJPEG. Cada imagen
  se codifica una sola vez por configuración de stream y el bucle casi no
  usa CPU.
- Una API HTTP definida en un perfil YAML, con autenticación Digest:
  snapshot, información del equipo y lectura y escritura de un parámetro.
- Analítica de video: líneas y regiones dibujadas sobre la imagen de la
  cámara, y eventos sobre ellas (cruce de línea, entrada y salida de región,
  permanencia, intrusión) disparados a mano o en momentos al azar, enviados
  a un destino con un POST HTTP. Cada entrega queda registrada con su estado
  y su latencia.
- Métricas por cámara (CPU, RAM, clientes). Si una cámara superaría el
  máximo de cámaras o los recursos del nodo, se rechaza indicando el motivo.
- Un panel que se actualiza en vivo por WebSocket.

## Requisitos

- Linux amd64 o arm64: Debian 12 o 13, Ubuntu 24.04 o posterior, o
  Raspberry Pi OS de 64 bits. El kernel necesita macvlan.
- **Red cableada**, para cámaras con MAC propia (macvlan): el Wi-Fi no
  acepta MACs extra, y los switches con port security también pueden
  bloquearlas. En una VM, el hipervisor tiene que permitir el modo promiscuo
  o el cambio de MAC. En Wi-Fi las cámaras usan la MAC del nodo (ipvlan,
  módulo del kernel `ipvlan`) y una IP estática.
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
administrador; no hay credenciales por defecto. Crearlo requiere el **código
de setup** de un solo uso del nodo, para que nadie que llegue antes al puerto
se quede con el nodo. Se imprime en el log y se guarda hasta que se usa:

```sh
docker compose logs mockvision | grep setup_code   # o bien:
docker compose exec -u mockvision mockvision cat /data/setup-code
```

Después:

1. **Perfiles → Importar perfil:** elige `profiles/milesight-demo.yaml`. Se
   valida y queda listado como *Borrador*.
2. **Destinos → Nuevo destino:** pon la URL que tiene que recibir los
   eventos, por ejemplo `http://192.168.1.10:8000/events`.
3. **Cámaras → Nueva cámara:** pon una IP libre de tu LAN y elige el
   destino. La cámara arranca y pasa a *Activa*.

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

Las cuentas de cámara tienen un rol: las `admin` y `operator` pueden cambiar
parámetros y las `viewer` solo leen (un perfil puede fijar los roles de cada
ruta). El usuario de la cámara es el que se cargó al crearla. Si la contraseña quedó
vacía, la cámara usa la cuenta de fábrica del perfil (`admin` / `ms1234`).
Pulsa **Disparar evento** en la cámara: el destino recibe el POST, y
**Eventos** muestra la entrega, el código HTTP y la latencia.

> **Prueba desde otro equipo.** Linux no deja que un equipo alcance sus
> propias interfaces macvlan. Por eso el nodo no puede abrir los streams de
> sus cámaras, y sus cámaras no pueden entregar eventos a un receptor que
> corra en el nodo. Pon el cliente y los destinos en otras máquinas, o
> activa **Configuración → Llegar a las cámaras desde este nodo** (ver
> [Red](#red)). La vista previa del snapshot en el panel funciona igual,
> porque no usa la red.

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

### Reglas y disparadores

Las reglas dicen dónde ocurren los eventos y los disparadores, cuándo; la
cámara no analiza su imagen. En la pestaña **Reglas** de la cámara se dibujan
sobre su instantánea:

- Una **línea** informa cruces. Sus lados son A (a la izquierda, yendo de su
  primer punto al segundo) y B; informa A → B, B → A o ambos.
- Una **región** informa lo que marques: entrada, salida, permanencia o
  intrusión.
- Cada regla puede detectar solo algunas clases de objeto (auto, persona…);
  ninguna significa todas las del perfil.

Un evento nombra su regla (ID, nombre y tipo), la dirección de un cruce y un
objeto con clase, color, confianza y un recuadro ubicado sobre la regla. Lo
que nadie le da, la cámara lo inventa.

- **A mano:** **Disparar evento** en la cabecera, los botones ⚡ de cada
  regla, o `POST /api/v1/cameras/{id}/events` con `type` y, si hace falta,
  `rule_id`, `direction`, `object`, `plate` y `speed`.
- **Al azar:** la pestaña **Disparadores** guarda disparadores que emiten un
  evento de un tipo, en una regla o en cualquiera habilitada, en un momento
  al azar entre dos esperas, mientras la cámara funciona. Pueden darles a
  sus eventos patentes, de una lista o generadas con formatos como `AA999AA`
  (9 un dígito, A una letra, X un dígito hexadecimal), y velocidades en un
  rango. **Disparar una vez** prueba uno.

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X PUT \
  -d '{"triggers":[{"name":"Tráfico","event_type":"line_crossing","min_seconds":5,"max_seconds":30}]}' \
  http://<nodo>:8080/api/v1/cameras/<id de la cámara>/triggers
```

Reglas y disparadores se aplican en el momento, sin reiniciar la cámara. El
perfil dice qué tipos de regla y qué objetos tiene la cámara, y cada cuánto
puede informar cada evento; un parámetro vinculado a `events.<tipo>.enabled`
apaga un tipo, como el interruptor de analítica de una cámara real (en el
demo, `Event.LineCrossing.Enable`). Los clones copian reglas y disparadores;
restaurar una cámara borra sus reglas y conserva sus disparadores, que desde
entonces disparan en cualquier regla. Sin una regla del tipo adecuado, los
eventos ocurren en una línea o región por defecto.

### Red

La pestaña **Red** de la cámara, y el diálogo de nueva cámara, eligen cómo
se une a la LAN. Los cambios se aplican cuando la cámara se reinicia; la
pestaña muestra la dirección que tiene ahora y de dónde salió.

| Modo | Para | Direccionamiento | A tener en cuenta |
|---|---|---|---|
| *macvlan* (por defecto) | redes cableadas | IP estática o DHCP | MAC propia, como un equipo real: el Wi-Fi y los puertos con port security la descartan |
| *ipvlan* | Wi-Fi, puertos de switch que admiten una sola MAC | IP estática | la MAC del nodo, así que los servidores DHCP no la distinguen |

Una placa de red lleva cámaras macvlan o ipvlan, no ambas, como exige el
kernel; **Llegar a las cámaras desde este nodo** cuenta como macvlan. El
panel nombra las cámaras que lo impiden.

- **DHCP.** La cámara se la pide al servidor de la LAN, como una cámara
  recién sacada de la caja, y renueva su concesión. Si ningún servidor
  responde en unos 15 segundos toma la dirección de fábrica de su perfil
  (`192.168.5.190` en el perfil demo) y sigue preguntando. Una renovación
  con otro router u otros servidores DNS se aplica en el momento; otra
  dirección, o una concesión perdida, reinicia la cámara.
- **DNS.** Los servidores de la cámara, si no los de la concesión, si no
  los del nodo.
- **Conflictos.** Una IP o una MAC que responde otro equipo detiene el
  arranque indicando qué equipo es. **Arrancar aunque otro equipo
  responda** se salta la comprobación, para probar cómo manejan los clientes
  un conflicto.
- **Salida.** Una cámara solo se conecta a los destinos de eventos, a sus
  servidores DNS y a DHCP; los clientes llegan a ella desde cualquier lado,
  como a una real.
- **Llegar a las cámaras desde el nodo.** **Configuración → Llegar a las
  cámaras desde este nodo** agrega una interfaz puente, `mv-bridge`, y una
  ruta a cada cámara macvlan, así funcionan los reproductores, grabadores y
  destinos del propio nodo. La interfaz padre necesita una dirección IPv4.
  Viene apagado porque cambia la red del nodo; las cámaras ipvlan quedan
  fuera del alcance del nodo de todas formas.

Una cámara que reintentar no arranca, por una placa ocupada o un kernel sin
ipvlan, deja de intentarlo y dice por qué; el panel muestra cada motivo en
su idioma.

### Configuración

`compose.yaml` pasa los dos ajustes que necesita la mayoría de las
instalaciones. El resto va en su sección `environment`.

| Variable | Por defecto | Qué es |
|---|---|---|
| `MOCKVISION_LISTEN` | `:8080` | Dirección del panel y de la API; `<ip>:<puerto>` para escuchar solo en una IP de gestión |
| `MOCKVISION_PARENT_IF` | interfaz de la ruta por defecto | Interfaz a la que se conectan las cámaras; se puede cambiar en **Configuración** del panel |
| `MOCKVISION_DATA` | `/data` | Base de datos, clave del nodo, imágenes y streams codificados |
| `MOCKVISION_FFMPEG` | `ffmpeg` | Binario de FFmpeg |
| `MOCKVISION_SECURE_COOKIES` | apagado | `1` detrás de un proxy inverso HTTPS |
| `MOCKVISION_ALLOWED_ORIGINS` | ninguno | Orígenes extra (`host:puerto`) que pueden llamar a la API |
| `MOCKVISION_ALLOWED_HOSTS` | cualquiera | Nombres de host a los que responde el panel (separados por comas); fíjalo para frenar el DNS rebinding. Las direcciones IP se aceptan siempre |
| `MOCKVISION_TRUSTED_PROXIES` | ninguno | Proxies inversos (IP o CIDR) cuyo `X-Forwarded-For` identifica al cliente, para los límites de inicio de sesión y la auditoría |
| `MOCKVISION_LOG_LEVEL`, `MOCKVISION_LOG_FORMAT` | `info`, texto | `debug`…`error`; `json` |
| `MOCKVISION_SERVICE_USER`, `MOCKVISION_CAMERA_USER` | `mockvision`, `mockvision-cam` | Usuarios del servicio principal y de las cámaras: nombres que deben existir, o uid numéricos; tienen que ser distintos |

Los límites (máximo de cámaras, umbrales de RAM y CPU, retención de eventos)
se configuran en **Configuración**.

### Sin Docker

Instala FFmpeg, compila (`make build`, requiere Go 1.27 y Node 22) e instala
el binario con la unidad de systemd de
[deploy/mockvision.service](deploy/mockvision.service); su encabezado lista
los pasos. Los usuarios del servicio y de las cámaras tienen que existir, y el
usuario de las cámaras tiene que poder ejecutar el binario (modo 0755): las
cámaras arrancan como ese usuario.

## Cómo funciona

Un único binario corre como tres tipos de proceso:

```
mockvision run      root, 9 capacidades     helper de red: namespaces, macvlan/ipvlan, sondeos, firewall, arranque de cámaras
 └─ mockvision serve   uid mockvision, ninguna   panel, API REST, WebSocket, SQLite, reconciliador
     └─ FFmpeg, validador de paquetes   confinados: seccomp y Landlock
 └─ mockvision camera  uid mockvision-cam, ninguna   una por cámara, en sus namespaces de red y de PID
```

- El **helper de red** es el único proceso con privilegios. Acepta un
  conjunto cerrado de pedidos validados del servicio, por un socket privado.
  Nunca ejecuta una shell y, una vez arrancado el servicio, vacía su conjunto
  de capacidades límite: nada de lo que lanza puede tener una capacidad.
- El **servicio principal** guarda el estado deseado en SQLite y lo
  reconcilia con el kernel al arrancar y cada 10 segundos. Las cámaras con
  autoarranque vuelven después de un reinicio.
- Cada **cámara** recibe sus sockets ya abiertos en su namespace y arranca
  directamente como el usuario de cámaras, nunca como root, en un namespace
  de PID propio. Antes de leer cualquier entrada fija `no_new_privs` y un
  filtro seccomp (sin namespaces, montajes, trazas, módulos ni llaveros), y
  Landlock limita sus archivos a sus streams y a lo que necesitan DNS y TLS.
  Después sirve los motores de su perfil (`rtsp`, `http-api`, `http-push`),
  corre sus disparadores aleatorios y habla con el servicio en líneas JSON;
  el servicio verifica lo que reporta contra el perfil, y guarda la regla y
  el disparador de un evento solo si son de esa cámara. Una cámara con DHCP
  pide su dirección ella misma, por un socket que le abrió el helper:
  interpreta lo que mandan los servidores sin privilegios, el servicio
  verifica la concesión y el helper la aplica.
- **Sondeos.** Antes de que una cámara tome una IP, el helper la sondea por
  ARP (RFC 5227); antes de que tome una MAC, la busca en las tablas y las
  interfaces del nodo, la pregunta por IPv6 y escucha un momento. El kernel
  rechaza una MAC que tenga otra interfaz del nodo aunque se salte el
  sondeo.
- **Firewall.** El namespace de cada cámara tiene una tabla nftables que
  deja salir un único conjunto de dirección, protocolo y puerto: sus
  destinos y sus servidores DNS en el puerto 53. Los nombres de los destinos
  se vuelven a resolver cada minuto con los servidores DNS de la cámara, y
  una dirección vista en los últimos diez minutos sigue permitida, para los
  nombres que rotan. También pasan DHCP, las respuestas a sus clientes, el
  RTP desde sus propios puertos y el loopback.
- **FFmpeg**, que decodifica las imágenes subidas, y el **validador de
  paquetes** también corren confinados: FFmpeg solo alcanza la imagen que lee
  y la rendition que escribe, y el validador ningún archivo. Ninguno de los
  dos puede leer la base de datos ni la clave del nodo.

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
| `make test-integration` | namespaces de red, macvlan, ipvlan, sondeo de MAC, firewall, socket DHCP y puente sobre un enlace virtual (root) |
| `make e2e` | los criterios de aceptación de la demo en una LAN virtual aislada (root, iproute2, ffmpeg, curl, ping, python3) |
| `make e2e-compose` | los mismos criterios contra la imagen Docker levantada con `compose.yaml` |
| `make generate` | consultas de sqlc y tipos de la API del panel desde `openapi.yaml` |

El binario se compila con `CGO_ENABLED=0`: soltar privilegios y el sandbox
cambian todos los hilos a la vez, y eso solo lo puede hacer un binario Go
puro.

## Seguridad

- La primera ejecución crea el administrador con un código de setup de un
  solo uso que aparece en el log del nodo. Las contraseñas usan Argon2id, con
  a lo sumo dos cálculos a la vez; cada dirección tiene diez intentos de
  inicio de sesión por minuto, y los fallos repetidos en una cuenta la
  bloquean para esa dirección.
- Las sesiones son cookies `HttpOnly` y `SameSite=Strict`. Todo pedido que
  cambia estado necesita un encabezado propio y pasa un chequeo de `Origin`.
  El panel corre con una CSP estricta y no carga nada de otros orígenes.
- Las contraseñas de cámaras y destinos se cifran con XChaCha20-Poly1305. La
  clave se guarda fuera de la base, y la API nunca las devuelve.
- Los tokens de API se guardan como hash SHA-256 y se muestran una sola vez.
  Tienen alcance (lectura o escritura), pueden vencer y se revocan al
  instante desde el panel, incluidos los WebSockets abiertos con ellos. Las
  sesiones del panel duran 12 horas sin uso y 7 días como máximo. La
  auditoría guarda cada cambio 90 días con su origen y su IP.
- Los clientes de la API emulada de una cámara también tienen límites: los
  cambios del codificador se agrupan por cámara, la cola de jobs admite a lo
  sumo 500 y las renditions sin uso se borran pasado un día.
- [docs/AUDITORIA.md](docs/AUDITORIA.md) es la auditoría de código de la que
  salen estas medidas, con la corrección de cada hallazgo.
- El panel es HTTP plano. Ponlo detrás de un proxy inverso HTTPS
  (`MOCKVISION_SECURE_COOKIES=1`) antes de exponerlo fuera de una red de
  laboratorio.

## Licencia

[Apache 2.0](LICENSE)
