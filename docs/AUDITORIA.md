# Auditoría de código — MockVision

- **Commit auditado:** `6dfc389` (rama `claude/code-audit-complete-orjodo`)
- **Fecha:** 2026-09-26
- **Alcance:** todo el repositorio: backend Go (`backend/`, `sdk/`, `database/`),
  panel React (`frontend/`), perfiles (`profiles/`), despliegue (`deploy/`,
  `compose.yaml`, `Makefile`, CI).

> **Estado:** los 25 hallazgos están corregidos en esta rama. La sección
> [Estado de las correcciones](#estado-de-las-correcciones), al final, dice
> qué se hizo en cada uno, en qué commit y cómo se verificó.

## Metodología

1. Lectura manual de todo el código fuente no generado, siguiendo los límites
   de confianza del diseño:
   `navegador → API/WS → servicio → helper root` y `LAN → proceso cámara → servicio`.
2. Herramientas:
   - `go vet ./...`: sin avisos.
   - `go test ./...`: todo pasa.
   - `go test -race ./backend/...`: todo pasa, sin carreras detectadas. Los
     tests unitarios no ejercitan el helper de red, que necesita root.
   - `npm audit --package-lock-only`: 0 vulnerabilidades.
   - `govulncheck`: **no se pudo ejecutar**, porque la política de red del
     entorno bloquea `vuln.go.dev`. Conviene correrlo en CI.
3. Pruebas de concepto para los hallazgos A1 y M2 (resultados en cada
   hallazgo).

## Escala de severidad

| Severidad | Criterio |
|---|---|
| **Alta** | Explotable desde la red por alguien sin credenciales del panel, con impacto serio sobre todo el nodo |
| **Media** | Hace falta una condición previa (perfil malicioso, credenciales de cámara, LAN compartida), o el impacto es parcial |
| **Baja** | Bug funcional, endurecimiento pendiente o impacto limitado |

## Resumen

| ID | Sev. | Hallazgo | Ubicación principal |
|---|---|---|---|
| A1 | Alta | DoS sin autenticar: Argon2id (64 MiB) sin límite de concurrencia en login y setup | `backend/internal/app/auth.go:92-139` |
| A2 | Alta | Codificaciones ilimitadas desde la LAN: cualquier cuenta de cámara (incluida `viewer` o la de fábrica) agota CPU, disco y cola de jobs | `backend/internal/app/cameras.go:1013` |
| M1 | Media | Setup inicial abierto al primero que llegue, también por DNS rebinding | `backend/internal/api/server.go:103,256` |
| M2 | Media | El timeout de las plantillas no detiene el render: CPU al 100 % y goroutines fugadas | `backend/internal/tmpl/tmpl.go:124-150` |
| M3 | Media | La protección contra fuerza bruta se puede vaciar o esquivar, y sirve para bloquear al admin | `backend/internal/app/auth.go:122,268-293` |
| M4 | Media | El WebSocket no se revalida tras logout, expiración o revocación del token | `backend/internal/api/ws.go:80` |
| M5 | Media | Los roles de las cuentas de cámara (`viewer`, `operator`) no se aplican | `backend/internal/engines/httpapi/engine.go:288` |
| M6 | Media | Helper root: `create` y `delete` sin sincronizar (carrera de datos, namespace e IP huérfanos) | `backend/internal/netctl/helper_linux.go:144-278` |
| B1 | Baja | El servicio confía en el IPC de la cámara, que es el proceso expuesto a la LAN | `backend/internal/app/supervisor.go:476-544` |
| B2 | Baja | Bomba de descompresión: imágenes de 16384×16384 (≈1 GiB por FFmpeg) | `backend/internal/media/media.go:458` |
| B3 | Baja | La cookie de sesión no se renueva: logout forzado a las 12 h aunque haya actividad | `backend/internal/api/server.go:309` |
| B4 | Baja | `state.get` con `from: form` no funciona (el cuerpo ya se leyó) | `backend/internal/engines/httpapi/engine.go:298,465` |
| B5 | Baja | Los eventos sin destinos quedan en "Entrega en curso…" para siempre | `backend/internal/app/events.go:197,215` |
| B6 | Baja | Digest: sin control de `nc` (replay durante 5 min) y se acepta `uri` sin query | `backend/internal/engines/httpapi/digest.go:149` |
| B7 | Baja | "Probar destino" sale desde el nodo, no desde la cámara (SSRF hacia localhost, diagnóstico engañoso) | `backend/internal/app/targets.go:198` |
| B8 | Baja | `lookupUser` cae en silencio a los UID 10001 y 10002 | `backend/internal/netctl/run_linux.go:134-150` |
| B9 | Baja | FFmpeg y el validador corren con el UID que lee la BD y `node.key` | `backend/internal/app/profiles.go:246`, `backend/internal/media/media.go:374` |
| B10 | Baja | Cámaras sin aislamiento de montaje, PID ni seccomp, y arrancan como root | `backend/internal/netctl/helper_linux.go:204` |
| B11 | Baja | `http.Server` del panel sin `ReadTimeout` ni `WriteTimeout` | `backend/cmd/mockvision/serve.go:133` |
| B12 | Baja | Choque de nombres de namespace al arrancar cámaras a la vez | `backend/internal/app/supervisor.go:617` |
| B13 | Baja | RTSP: carrera en `cfg.Paths`, y `OnPlay` no vuelve a autenticar | `backend/internal/engines/rtsp/engine.go:180,395` |
| B14 | Baja | Recursos que nunca se liberan: mapas en memoria, renditions de assets borrados, uploads interrumpidos | `backend/internal/app/service.go:256`, `backend/internal/app/assets.go:171,272` |
| B15 | Baja | La retención usa el reloj de la cámara, que los clientes pueden desplazar | `backend/internal/app/events.go:71` |
| B16 | Baja | `LocalRuntime` crea el socketpair sin `SOCK_CLOEXEC` | `backend/internal/netctl/local.go:60` |
| B17 | Baja | Cadena de suministro y despliegue: imágenes y Actions sin fijar; unidad systemd poco restringida | `deploy/Dockerfile`, `.github/workflows/ci.yml`, `deploy/mockvision.service` |

---

## Hallazgos de severidad alta

### A1. DoS sin autenticar por memoria: Argon2id sin límite de concurrencia

**Ubicación:** `backend/internal/app/auth.go:92-139` (`Setup`, `Login`) y
`backend/internal/api/server.go:103-104` (rutas públicas).

**Descripción.** Cada hash Argon2id reserva 64 MiB (`argonMemory = 64*1024`) y
nada limita cuántos se calculan a la vez:

- `Login` calcula el hash aunque el usuario no exista (`dummyHash`, para
  igualar tiempos). El bloqueo va por `usuario|IP`, así que cambiar el nombre
  de usuario en cada petición lo esquiva por completo.
- `Setup` calcula `hashPassword(password)` **antes** de comprobar si el setup
  ya se hizo (`auth.go:99` frente a `auth.go:104-112`). Cualquier
  `POST /api/v1/auth/setup` con una contraseña de 10 caracteres o más cuesta
  un Argon2 completo, sin bloqueo ni límite, y después responde 409.

**Prueba de concepto.** 16 llamadas concurrentes a `argon2.IDKey` con los
parámetros del proyecto llevaron el pico de memoria (`VmHWM`) a **1 065 592 kB
(≈1 GiB)**. En una Raspberry Pi 4, que es el objetivo declarado, bastan unas
decenas de peticiones concurrentes para que el OOM killer mate a `serve`.
Cuando `serve` muere, `RunMain` retorna y `helper.Shutdown()` detiene **todas
las cámaras** (`run_linux.go:95-112`).

**Recomendación.**
- Un semáforo global para Argon2 (1-2 cálculos a la vez). Si está ocupado,
  responder `429`/`503` con `Retry-After` en lugar de encolar sin límite.
- En `Setup`, comprobar `CountUsers` antes de calcular el hash.
- Limitar por IP independientemente del usuario (token bucket), además del
  bloqueo por cuenta.
- Considerar parámetros más ligeros en el hardware objetivo, como la
  recomendación de OWASP de m=19 MiB, t=2, p=1.

### A2. Codificaciones ilimitadas desde la LAN con cualquier cuenta de cámara

**Ubicación:** la ruta `param.cgi?action=set` del perfil de demo recorre
`backend/internal/engines/httpapi/engine.go:506` (`stateSet`),
`backend/internal/app/supervisor.go:529` (`clientChanges`),
`backend/internal/app/cameras.go:1013-1024` (`afterChanges` y
`regenerateStreamsLater`), `backend/internal/app/assets.go:179`
(`ensureRenditionRow`) y `backend/internal/worker/runner.go` (`Submit`).

**Descripción.** Un cliente de la API emulada que cambia un parámetro ligado a
`media.*` (códec, resolución, fps, bitrate, GOP) crea una rendition nueva por
cada combinación distinta y encola un job de FFmpeg. No existe:

- límite de frecuencia por cámara ni por cliente;
- límite de tamaño de la cola de jobs. Los jobs `queued` se persisten, así que
  la cola **sobrevive a un reinicio**;
- recolección de basura de renditions: ni las filas de `renditions` ni los
  directorios de `data/renditions/` se borran nunca (no hay ninguna consulta
  `DELETE FROM renditions`);
- control de rol (ver M5): una cuenta `viewer` basta, y la cuenta de fábrica
  `admin/ms1234` se usa cuando no se define contraseña.

Solo con el perfil de demo, el stream `main` admite 2 × 3 × 30 × 7937 × 300
≈ 4,3·10⁸ combinaciones. Además, cada llamada a `regenerateStreamsLater` lanza
una goroutine que espera el job hasta 10 minutos.

**Impacto.** Cualquier equipo de la LAN que conozca una contraseña de cámara,
débil por diseño, mantiene el CPU del nodo saturado de forma indefinida: el
control de admisión empieza a rechazar cámaras y los streams sufren. Además,
el disco, la base de datos y la cola crecen sin límite. Un VMS legítimo que
barra configuraciones puede provocarlo sin querer.

**Recomendación.**
- Limitar la frecuencia de cambios `media.*` por cámara, colapsando los que
  llegan en ráfaga y quedándose con el último.
- Poner un tope a los jobs abiertos y rechazar o descartar los que excedan.
- Recolectar renditions sin uso: las que no referencia ningún
  `camera_streams` y llevan más de N horas, con filas y archivos.
- Aplicar los roles de las cuentas de cámara (M5).

---

## Hallazgos de severidad media

### M1. Setup inicial abierto al primero que llegue, también por DNS rebinding

**Ubicación:** `backend/internal/api/server.go:103` (`POST /auth/setup` es
público), `backend/internal/api/server.go:256-268` (`originAllowed` compara
contra `r.Host`) y `compose.yaml` (`network_mode: host` y `:8080` en todas las
interfaces).

**Descripción.** Hasta que alguien crea el administrador, cualquiera que
alcance el puerto puede crearlo y quedarse con el nodo. `GET /auth/me` revela
`setup_required`. La comprobación CSRF compara `Origin` con la cabecera `Host`,
que controla quien hace la petición, y el servidor no valida `Host` contra una
lista. Con DNS rebinding, una web externa que visite alguien de la LAN puede
completar el setup desde el navegador de esa persona: la petición parece del
mismo origen y puede enviar `X-MockVision-Request`.

**Recomendación.** Un token de setup de un solo uso, impreso en los logs al
arrancar y exigido por `/auth/setup` (como hacen Jupyter o Grafana), o limitar
el setup a loopback. Además, una lista opcional de `Host` permitidos
(`MOCKVISION_ALLOWED_HOSTS`) para frenar el DNS rebinding en todas las rutas.

### M2. El timeout de las plantillas no detiene el render

**Ubicación:** `backend/internal/tmpl/tmpl.go:124-150`.

**Descripción.** `Render` ejecuta la plantilla en una goroutine y, cuando pasan
50 ms, marca `abort` y retorna. `abort` solo se comprueba en `Write`, así que
una plantilla que itera sin escribir sigue corriendo para siempre.
`text/template` no se puede cancelar, y desde Go 1.22 admite `range` sobre un
entero.

**Prueba de concepto.** Con `{{range 2000000000}}{{end}}`, `Render` devolvió
`exceeded 50ms` a los 50,6 ms, pero la goroutine seguía viva 500 ms después
(`goroutines before=2 after=3`) y consumiendo un núcleo.

**Impacto.** Un perfil de terceros (los paquetes no se firman ni se verifican,
según `DEMO-NOTES.md`) puede incluir una ruta así. Cada petición de un cliente
de la LAN a esa ruta fuga una goroutine que ocupa un núcleo hasta agotar el
CPU del nodo. El paquete promete que "each render is bounded in time", y no
se cumple.

**Recomendación.** Rechazar en el importador `range` sobre enteros o sobre
valores no acotados; es posible recorriendo el `parse.Tree`. Alternativa o
complemento: una función de conteo de pasos inyectada en los `range`
(reescribiendo el árbol), o renderizar en un subproceso con `RLIMIT_CPU`. Como
mínimo, limitar las renders simultáneas por cámara con un semáforo, para que
las goroutines fugadas no se acumulen sin fin.

### M3. La protección contra fuerza bruta se puede vaciar o esquivar

**Ubicación:** `backend/internal/app/auth.go:122-139` y `268-293`, y
`backend/internal/api/server.go:86` (`clientIP`).

**Descripción.**
1. La clave del bloqueo es `usuario|IP`: con varias IPs, o un /64 de IPv6, cada
   dirección tiene 4 intentos libres.
2. `fail()` vacía **todo** el mapa cuando supera 10 000 entradas. Basta con
   10 001 fallos con usuarios inventados para borrar el bloqueo de `admin` y
   seguir probando.
3. Detrás del proxy inverso que recomienda el README, todos los clientes
   comparten la IP del proxy (no se lee `X-Forwarded-For`): un atacante puede
   mantener bloqueado al administrador legítimo, hasta 15 minutos
   renovables.

**Recomendación.** Desalojar solo las entradas antiguas (LRU o TTL), nunca
vaciar el mapa entero. Añadir un límite por cuenta con retardo progresivo que
no bloquee en seco, y un límite por IP. Soportar una lista de proxies de
confianza para leer `X-Forwarded-For`.

### M4. El WebSocket no se revalida tras logout, expiración o revocación

**Ubicación:** `backend/internal/api/ws.go:80-152`.

**Descripción.** La sesión o el token se comprueban solo al abrir el
WebSocket. Si después se revoca el token, caduca, se cierra la sesión o se
deshabilita el usuario, la conexión sigue recibiendo en vivo los eventos, la
auditoría (con IPs y diffs), los cambios de configuración, etc. El README
afirma que los tokens "are revoked at once", y para el WebSocket no es así.

**Recomendación.** Revalidar la sesión o el token en cada ping (cada 20 s) y
cerrar con `StatusPolicyViolation` si ya no es válida. Otra opción: que
`RevokeToken` y `Logout` avisen al hub para desconectar a los clientes
asociados.

### M5. Los roles de las cuentas de cámara no se aplican

**Ubicación:** `backend/internal/engines/httpapi/engine.go:288-330`
(autenticación y después cualquier ruta, incluido `state.set`) y
`backend/internal/engines/rtsp/engine.go:322` (`authorize`).

**Descripción.** Las cuentas tienen rol (`admin`, `operator`, `viewer`), que
se valida, se guarda y se muestra, pero ningún motor lo consulta. Un `viewer`
puede cambiar la configuración de la cámara y disparar recodificaciones (A2).
Eso no refleja cómo se comporta un dispositivo real, y es justo lo que un
cliente bajo prueba necesita comprobar.

**Recomendación.** Añadir a las rutas del perfil un campo `roles` (o
`min_role`) y aplicarlo en `ServeHTTP`. Mientras tanto, que `state.set` exija
`admin` u `operator` por defecto.

### M6. Helper root: `create` y `delete` sin sincronizar

**Ubicación:** `backend/internal/netctl/helper_linux.go:99` (`go h.handle`, una
goroutine por petición), `144-200` (`create` escribe `cp.ns` y `cp.cmd` sin
`h.mu`) y `250-278` (`delete` los lee sin `h.mu`).

**Descripción.**
1. **Carrera de datos:** `list()` lee `cp.ns` y `cp.cmd` bajo `h.mu`, pero
   `create` los escribe sin el lock. El reconciliador llama a `Live` cada 10 s,
   así que coincide con cualquier arranque de cámara.
2. **Recurso huérfano:** si llega un `camera.delete` mientras hay un `create`
   en curso, `delete` encuentra `cp.cmd == nil` y `cp.ns == nil`, no mata nada y
   borra la entrada del mapa. `create` sigue: crea el namespace, toma la IP en
   la LAN, anuncia ARP y arranca el proceso, pero ya nadie lo rastrea. El
   namespace con nombre queda montado en `/run/netns/sim-*`, con la macvlan
   respondiendo a esa IP, y el siguiente arranque de esa cámara falla con "already
   exists" hasta que se reinicia el nodo. El servicio actual no suele provocar
   esta secuencia, pero el helper declara que "trusts nothing".
3. `go h.handle` no tiene límite de concurrencia.

**Recomendación.** Proteger `cp.ns` y `cp.cmd` con `h.mu`. Si `delete` llega
durante un `create`, marcar `deleting` y que `create`, al terminar cada paso,
compruebe la marca y limpie lo que haya creado. Otra opción es serializar por
cámara con un mutex por ID. Limitar las peticiones en vuelo.

---

## Hallazgos de severidad baja

### B1. El servicio confía en el IPC de la cámara

**Ubicación:** `backend/internal/app/supervisor.go:476-544`,
`backend/internal/app/events.go:71-115` y
`backend/internal/telemetry/telemetry.go:264`.

La cámara es el proceso que analiza tráfico de la LAN; el diseño le quita los
privilegios precisamente por eso. Aun así, el servicio acepta sus mensajes sin
validarlos:
- **heartbeats** sin límite de frecuencia: `Cameras.Add` guarda las muestras
  por tiempo (10 min), no por cantidad, así que una cámara comprometida puede
  agotar la memoria del servicio. RSS y CPU falsos distorsionan además la
  admisión;
- **`state.changed`**: `clientChanges` persiste claves y valores arbitrarios en
  `camera_state` sin validarlos contra el perfil, y confía en `Bind` para
  decidir si regenera streams;
- **`delivery`**: `recordDelivery` no comprueba que `EventID` pertenezca a esa
  cámara, así que puede escribir entregas en eventos de otras cámaras;
- **`event`**: el ID y el tamaño los decide la cámara (un ID `ZZZ…` queda
  siempre primero en la lista);
- `log`, `gap` y `client` se publican en el hub sin límite de tamaño ni de
  frecuencia. El topic `camera:<id>` guarda 200 mensajes de hasta 1 MB.

**Recomendación.** Validar contra el perfil en el servicio (claves, tipos, bind
calculado por el servicio), limitar la frecuencia por tipo de mensaje,
comprobar que el evento es de la cámara y limitar el tamaño de lo que se
publica y se guarda.

### B2. Bomba de descompresión de imágenes

**Ubicación:** `backend/internal/media/media.go:458`.

Se aceptan imágenes de hasta 16384×16384. Un PNG de color plano de ese tamaño
ocupa poco, pero FFmpeg lo decodifica en ≈1 GiB por proceso, con hasta 16 jobs
concurrentes (`max_jobs`). Como la rendition máxima es 7680×4320, conviene
limitar a unos 33 MP y, de paso, forzar el demuxer (`-f image2` o
`-f png_pipe`/`jpeg_pipe`) con `-protocol_whitelist file`.

### B3. La cookie de sesión no se renueva

**Ubicación:** `backend/internal/api/server.go:309` y
`backend/internal/app/auth.go:165-183`.

`Authenticate` extiende la sesión en la base de datos, pero la cookie solo se
fija en el login con `Expires = login + 12 h`. Pasado ese tiempo, el navegador
la descarta aunque haya actividad y el usuario tiene que volver a entrar.
Conviene volver a emitir la cookie cuando se extiende la sesión, o usar una
cookie de sesión sin `Expires` con límite en el servidor. Recomendable también
una vida máxima absoluta de la sesión.

### B4. `state.get` con `from: form` no funciona

**Ubicación:** `backend/internal/engines/httpapi/engine.go:298` y `465`.

`readBody` consume `r.Body` antes de la acción, y `stateGet` usa
`r.PostFormValue`, que lee un cuerpo ya vacío. La clave siempre llega vacía y
la ruta devuelve **todos** los parámetros. Hay que parsear
`url.ParseQuery(req.Body)`, como ya hace `stateSet`.

### B5. Eventos sin destinos quedan en "Entrega en curso…"

**Ubicación:** `backend/internal/app/events.go:197` y `215-222`.

`eventView` llama a `deliveryStatus(dels, -1)`. Sin entregas y con
`targets == -1`, el resultado es `pending`, así que el evento de una cámara
sin destinos (o sin destinos para ese tipo) se ve para siempre como
"Delivery in progress…" (`frontend/src/components/EventTable.tsx:92`). La rama
`none` solo se alcanza en la respuesta inmediata de `TriggerEvent`. Hay que
guardar con el evento cuántos destinos le correspondían, o calcularlo al
listar.

### B6. Digest: replay y `uri` sin query

**Ubicación:** `backend/internal/engines/httpapi/digest.go:29` y `149`.

Los nonces no llevan estado y no se controla `nc`, así que una cabecera
capturada se puede reutilizar durante 5 minutos. Además, se acepta `uri` igual
a `r.URL.Path` aunque la petición traiga query, y `ha2` se calcula sobre ese
`uri`: una cabecera firmada para `/param.cgi` vale también para
`/param.cgi?action=set&…`. Solo debería aceptarse `uri == r.RequestURI`.

### B7. "Probar destino" sale desde el nodo, no desde la cámara

**Ubicación:** `backend/internal/app/targets.go:198`.

La prueba se hace desde el namespace del servicio, que es el del host. Puede
alcanzar `127.0.0.1` y servicios internos del nodo (SSRF con un token `write`),
e informa del estado y los errores de conexión, lo que sirve como escáner de
puertos. Además, diagnostica algo distinto de lo que hacen las cámaras: desde
su macvlan no pueden alcanzar el nodo. Conviene hacer la prueba desde una
cámara en marcha o, al menos, rechazar destinos de loopback o link-local y
advertir de la diferencia.

### B8. `lookupUser` cae en silencio a UID fijos

**Ubicación:** `backend/internal/netctl/run_linux.go:134-150` y `46`.

Si `mockvision` o `mockvision-cam` no existen, se usan 10001 y 10002 sin
avisar. Esos UID pueden pertenecer a usuarios reales del host, que pasarían a
ser dueños de la base de datos y de `node.key`. Además, que servicio y cámaras
compartan UID solo genera un aviso. Debería ser un error, salvo que se pida
explícitamente con un UID numérico.

### B9. FFmpeg y el validador comparten UID con los datos

**Ubicación:** `backend/internal/app/profiles.go:246` y
`backend/internal/media/media.go:374`.

Está documentado en `DEMO-NOTES.md`, pero conviene tenerlo presente: el
subproceso de validación de paquetes y FFmpeg, que decodifica imágenes
subidas, corren con el UID del servicio, que puede leer `db.sqlite` y
`node.key`. El subproceso solo aísla de *crashes*, no de un compromiso. Se
recomienda un UID propio, o Landlock y seccomp, para ambos.

### B10. Cámaras sin aislamiento de montaje, PID ni seccomp, y arranque como root

**Ubicación:** `backend/internal/netctl/helper_linux.go:204-222` y
`backend/internal/camera/main.go:40`.

El proceso de cámara se ejecuta como root con capacidades y baja privilegios
él mismo. Durante el arranque del runtime y el parseo de flags sigue siendo
root. `SysProcAttr.Credential` (y `AmbientCaps` vacío) lo evitaría. Después,
las cámaras comparten el sistema de archivos y el espacio de PID del host y
el mismo UID, así que una cámara puede enviar señales a las demás. Conviene
añadir un namespace de montaje (y de PID) y un filtro seccomp.

### B11. `http.Server` del panel sin `ReadTimeout` ni `WriteTimeout`

**Ubicación:** `backend/cmd/mockvision/serve.go:133`.

Solo hay `ReadHeaderTimeout`. Un cliente sin autenticar puede mantener muchas
conexiones enviando el cuerpo de `/auth/login` muy despacio (el decodificador
lee hasta 1 MB). Conviene fijar plazos por petición con
`http.ResponseController` (excepto para el WebSocket), o un `ReadTimeout`
global con excepción para `/api/v1/ws`.

### B12. Choque de nombres de namespace al arrancar cámaras a la vez

**Ubicación:** `backend/internal/app/supervisor.go:617`.

`netnsName` solo mira los `ss.netns` ya asignados, y se asignan después del
`Launch`. Dos cámaras cuyo slug coincide (nombres truncados a 30 caracteres,
solo mayúsculas o puntuación distintas, o nombres sin letras latinas, que dan
`sim-cam`) y que arrancan a la vez piden el mismo nombre: una falla con
"already exists" y entra en backoff. Conviene reservar el nombre bajo `s.mu`
antes del `Launch`.

### B13. RTSP: carrera en `cfg.Paths`, y `OnPlay` no vuelve a autenticar

**Ubicación:** `backend/internal/engines/rtsp/engine.go:180` y `395`.

El callback de `Media().Watch` lee `e.cfg.Paths` sin `e.mu`, mientras `Reload`
lo escribe con el lock. `OnPlay` no llama a `authorize`: depende de que el
`SETUP` de esa sesión se autenticara. Además, `playing` es global al motor, así
que un espectador de un stream hace que se empaqueten todos.

### B14. Recursos que nunca se liberan

- `s.opLocks`, `s.encodes` y `s.exits` crecen con cada cámara o rendition y
  nunca se podan (`backend/internal/app/service.go:256`,
  `backend/internal/app/assets.go:272`).
- `DeleteAsset` borra el archivo del asset, pero no los directorios de sus
  renditions (`backend/internal/app/assets.go:171`); las filas se borran en
  cascada.
- Los uploads de imports que quedan `interrupted` y no se reanudan se quedan
  en `data/jobs/`.

### B15. La retención usa el reloj de la cámara

**Ubicación:** `backend/internal/app/events.go:71`.

`event.at` sale del reloj de la cámara más `time.offset`, que los clientes de
la API emulada pueden cambiar. La retención borra por `at`: un desfase
negativo hace que los eventos se borren enseguida y uno positivo, que no se
borren nunca. Conviene guardar además la hora de recepción del servicio y
usarla para la retención.

### B16. `LocalRuntime` crea el socketpair sin `SOCK_CLOEXEC`

**Ubicación:** `backend/internal/netctl/local.go:60`.

Entre `Socketpair` y `CloseOnExec`, un `fork` concurrente (FFmpeg u otra
cámara) puede heredar el descriptor, y entonces la cámara no ve EOF cuando el
servicio cierra. Solo afecta al modo de desarrollo. Basta con usar
`SOCK_CLOEXEC`, como ya hace `HelperRuntime`.

### B17. Cadena de suministro y despliegue

- `deploy/Dockerfile` usa `ubuntu:24.04`, `golang:1.27` y `node:22-slim` sin
  fijar por digest.
- `.github/workflows/ci.yml` referencia las Actions por tag (`@v5`, `@v6`), no
  por SHA.
- `deploy/mockvision.service` no usa `ProtectSystem`, `ProtectHome`,
  `PrivateTmp`, `ProtectKernelModules` ni `SystemCallFilter`: el helper es root
  con `CAP_SYS_ADMIN` y puede escribir en todo el sistema. Hay un compromiso
  documentado, porque un namespace de montaje privado impediría ver
  `/run/netns` desde el host, pero `ProtectKernelModules`,
  `ProtectKernelLogs`, `ProtectClock`, `SystemCallFilter=@system-service @mount`,
  etc. no crean un namespace de montaje y se pueden añadir.
- Añadir `govulncheck` al CI.

---

## Aspectos positivos

- **SQL:** todas las consultas las genera sqlc con parámetros; no se encontró
  SQL dinámico con datos de entrada.
- **CSRF y cabeceras:** cookies `HttpOnly` y `SameSite=Strict`, cabecera
  personalizada obligatoria, comprobación de `Origin`, CSP estricta sin
  `unsafe-inline`, `frame-ancestors 'none'` y `nosniff`.
- **Secretos:** XChaCha20-Poly1305 con contexto autenticado por fila; la API
  nunca devuelve contraseñas; la auditoría registra qué cuentas cambiaron de
  contraseña, nunca el valor.
- **Tokens de API:** 256 bits aleatorios, guardados como SHA-256, con prefijo
  `mvt_`, alcance de lectura o escritura, y sin poder gestionar otros tokens.
- **Importación:** límites de tamaño, número de archivos, ratio de compresión
  y rutas seguras en el zip; YAML con límites de nodos, profundidad y alias;
  JSON Schema y validación en un subproceso.
- **Helper de red:** valida todos los campos con expresiones regulares, no usa
  shell, parsea ARP con comprobación de límites, y las cámaras verifican que
  no queda ninguna capacidad en ningún hilo (`privdrop.Verify`).
- **Panel:** React sin `dangerouslySetInnerHTML` ni otros sumideros de XSS;
  `npm audit` sin vulnerabilidades.
- **Calidad:** `go vet` limpio y tests que pasan, también con `-race`.

## Plan de remediación sugerido

1. **Inmediato:** A1 (semáforo de Argon2, y en `Setup` comprobar antes de
   calcular el hash) y M1 (token de setup).
2. **Corto plazo:** A2 y M5 (límite de frecuencia y de cola, recolección de
   renditions, roles), M2 (rechazar `range` no acotado), M3, M4 y M6.
3. **Después:** B1 a B17, empezando por los bugs funcionales B3, B4 y B5, que
   el usuario nota.

---

## Estado de las correcciones

Todos los hallazgos se corrigieron en la rama `claude/code-audit-complete-orjodo`,
en este orden. Cada commit lleva sus tests.

| ID | Corrección | Commit |
|---|---|---|
| A1 | Argon2id corre de a dos como máximo, con una cola corta (si se llena, `503` con `Retry-After`); el setup responde "ya hecho" antes de calcular el hash | `9ff291b` |
| A2 | Los cambios del codificador de una cámara se agrupan: una regeneración a la vez y, como mucho, una más con los últimos valores. La cola admite hasta 500 jobs, y un recolector borra cada hora las renditions sin uso de más de un día, con sus archivos y los directorios huérfanos | `9ff291b` |
| M1 | Crear el administrador requiere un código de un solo uso, impreso en el log y guardado en `<data>/setup-code` hasta usarse. Nueva variable opcional `MOCKVISION_ALLOWED_HOSTS` contra el DNS rebinding | `9ff291b` |
| M2 | Cada `range` y cada llamada a `template` pasan por un presupuesto de 100 000 pasos por render, que además corta el render cuando vence su tiempo | `9ff291b` |
| M3 | Cada dirección tiene 10 intentos por minuto sea cual sea el usuario (IPv6 por /64). La tabla de bloqueos desaloja entradas viejas en vez de vaciarse. Nueva variable `MOCKVISION_TRUSTED_PROXIES` para leer `X-Forwarded-For` | `9ff291b` |
| M4 | El WebSocket se revalida en cada ping sin extender la sesión, y se cierra al instante al revocar el token o cerrar la sesión | `9ff291b` |
| M5 | Las rutas del perfil aceptan `roles`; `state.set` exige `admin` u `operator` por defecto | `9ff291b` |
| M6 | El helper protege el namespace y el proceso de cada cámara con su mutex; un `delete` durante un `create` espera y limpia lo creado. Hay un máximo de 64 pedidos a la vez | `73576c6`, `245a47d` |
| B1 | El servicio vuelve a validar lo que reporta la cámara: eventos (ULID reciente, tipo del perfil, 64 KiB), entregas (evento y destino de esa cámara), cambios de parámetros (validados contra el perfil), heartbeats (uno por segundo, valores acotados, 600 muestras) y avisos (con presupuesto y tamaño) | `788bd95` |
| B2 | Imágenes de 33 megapíxeles como máximo; FFmpeg lee con `-f image2 -protocol_whitelist file` | `94fb343` |
| B3 | La cookie se reenvía cuando se extiende la sesión, y las sesiones duran 7 días como máximo | `75f2e0e` |
| B4 | `state.get` con `from: form` lee el cuerpo que ya se había leído | `75f2e0e` |
| B5 | Los eventos guardan cuántas entregas esperan (migración `00004`) | `788bd95` |
| B6 | En Digest, `uri` tiene que ser la petición completa, y cada `nonce`/`nc`/`cnonce` se acepta una sola vez | `75f2e0e` |
| B7 | La prueba de un destino sale de una cámara en marcha que lo usa (mensaje IPC `target.test`). Si no hay ninguna, sale del nodo, que rechaza loopback, link-local, multicast y sus propias direcciones | `9efc880` |
| B8 | Si el usuario del servicio o de las cámaras no existe, es un error, y los dos tienen que ser distintos | `9efc880` |
| B9 | FFmpeg pasa por `mockvision sandbox-exec` (seccomp y Landlock limitados a la imagen y al directorio de salida); el validador se confina sin acceso a archivos | `94fb343`, `8815396` |
| B10 | El helper vacía su conjunto de capacidades límite y lanza cada cámara directamente como su usuario, en un namespace de PID propio. La cámara fija seccomp y Landlock (nuevo paquete `internal/sandbox`) | `73576c6` |
| B11 | Plazos de lectura y escritura por petición, excepto en el WebSocket | `75f2e0e` |
| B12 | Los nombres de namespace se reservan mientras dura la sesión de la cámara | `9efc880` |
| B13 | RTSP: `cfg.Paths` se lee con lock; `PLAY` exige un `SETUP` autorizado o credenciales propias; los espectadores se cuentan por stream | `75f2e0e` |
| B14 | Una cámara borrada no deja estado en memoria; los locks de codificación se liberan; los directorios de renditions de un asset se borran con él; los jobs interrumpidos y abandonados se cancelan y limpian | `9ff291b`, `9efc880` |
| B15 | Los eventos guardan la hora de recepción, y la retención usa esa hora | `788bd95` |
| B16 | El socketpair del modo local se crea con close-on-exec | `73576c6` |
| B17 | Imágenes base fijadas por digest, Actions por commit, `govulncheck` y `npm audit` en el CI, Dependabot y la unidad systemd endurecida (pasa `systemd-analyze verify`) | `428aef8` |

### Hallazgos nuevos durante las correcciones

- **Arranque:** el inicio del nodo y la primera cámara podían crear a la vez
  la imagen de prueba incorporada usando el mismo archivo temporal, y uno lo
  borraba mientras el otro lo leía. Ahora hay un lock y cada uno usa un
  archivo propio (`94fb343`).
- **IP tras borrar una cámara:** el descriptor reservado para el segundo ARP
  gratuito mantenía vivo el namespace hasta un segundo, así que una cámara
  borrada justo después de arrancar podía seguir respondiendo en su IP e
  incluso volver a anunciarla. Ahora el borrado elimina la interfaz y cancela
  ese anuncio (`245a47d`). Lo detectó el e2e.

### Verificación

- `go vet`, los tests unitarios (también con `-race`), el chequeo de tipos y
  traducciones del panel y su build: en verde. Cada hallazgo tiene su test,
  salvo los de despliegue (B17), que se verifican con `systemd-analyze verify`
  y el build de la imagen.
- Tests de integración como root (namespaces y macvlan reales). M6 tiene un
  test determinista que falla con el helper anterior y pasa con el nuevo.
- E2E completo en una LAN virtual (`make e2e`), con el binario y con la
  imagen Docker levantada con `compose.yaml`: 37 de 37 comprobaciones,
  incluidas las nuevas (código de setup, rol `viewer`, seccomp y namespace
  de PID de la cámara).
- `govulncheck` sigue sin poder ejecutarse en este entorno por la política de
  red; desde ahora corre en el CI.

