/**
 * Spanish translations for the panel. The English source string is the key,
 * so any string not listed here falls back to English. `{name}` placeholders
 * are filled by t() at call time. Only the panel's own copy is translated;
 * messages that come from the node (API errors, profile ids, role names,
 * signature and validation output) are shown as the node returns them.
 */
export const es: Record<string, string> = {
  // Navigation and shell
  Cameras: "Cámaras",
  Events: "Eventos",
  Profiles: "Perfiles",
  Assets: "Imágenes",
  Targets: "Destinos",
  Settings: "Configuración",
  "Log out": "Cerrar sesión",
  Live: "En vivo",
  Connecting: "Conectando",
  Offline: "Sin conexión",
  "local mode": "modo local",
  "local mode (127.0.0.1)": "modo local (127.0.0.1)",
  "parent {iface}": "padre {iface}",
  "{running} running / {total}": "{running} activas / {total}",
  Language: "Idioma",
  "pending changes": "cambios pendientes",

  // Common controls
  Save: "Guardar",
  "Saving…": "Guardando…",
  Discard: "Descartar",
  Cancel: "Cancelar",
  Delete: "Eliminar",
  Remove: "Quitar",
  Close: "Cerrar",
  Reset: "Restablecer",
  Dismiss: "Descartar",
  Name: "Nombre",
  Username: "Usuario",
  Password: "Contraseña",
  Profile: "Perfil",
  Role: "Rol",
  Port: "Puerto",
  Enabled: "Habilitado",
  Actions: "Acciones",
  Yes: "Sí",
  No: "No",
  none: "ninguna",
  Default: "Por defecto",
  default: "por defecto",
  "Default ({iface})": "Por defecto ({iface})",
  "no address": "sin dirección",

  // Camera actions (shared by list and detail)
  "New camera": "Nueva cámara",
  "Line crossing": "Cruce de línea",
  "Send a line-crossing event to the camera's targets": "Envía un evento de cruce de línea a los destinos de la cámara",
  "Line crossing sent from {name}": "Cruce de línea enviado desde {name}",
  Start: "Iniciar",
  Stop: "Detener",
  Restart: "Reiniciar",
  Clone: "Clonar",
  Restore: "Restaurar",
  "Start {name}": "Iniciar {name}",
  "Stop {name}": "Detener {name}",
  "Clone {name}": "Clonar {name}",
  "Delete {name}": "Eliminar {name}",
  "Delete camera {name} and its events?": "¿Eliminar la cámara {name} y sus eventos?",
  "{name} is restarting": "{name} se está reiniciando",

  // Login
  "Create the administrator": "Crea el administrador",
  "Sign in": "Iniciar sesión",
  "This node has no users yet. The first account manages everything; there are no default credentials.":
    "Este nodo todavía no tiene usuarios. La primera cuenta administra todo; no hay credenciales por defecto.",
  "Sign in to manage the simulated cameras of this node.":
    "Inicia sesión para administrar las cámaras simuladas de este nodo.",
  "At least 10 characters.": "Al menos 10 caracteres.",
  "Repeat password": "Repite la contraseña",
  "The passwords do not match.": "Las contraseñas no coinciden.",
  "Create and sign in": "Crear e iniciar sesión",

  // Cameras page
  "Simulated IP cameras on this node. Each one answers on the LAN as its profile describes.":
    "Cámaras IP simuladas en este nodo. Cada una responde en la LAN según describe su perfil.",
  "Loading cameras…": "Cargando cámaras…",
  "No cameras yet": "Todavía no hay cámaras",
  "Import a profile, then create a camera with a free IP address of your LAN.":
    "Importa un perfil y luego crea una cámara con una dirección IP libre de tu LAN.",
  Address: "Dirección",
  State: "Estado",
  Clients: "Clientes",
  "restart pending": "reinicio pendiente",
  "Saved {what} changes apply after a restart": "Los cambios guardados de {what} se aplican tras reiniciar",

  // Events page and table
  "Events sent by the cameras and the delivery to their targets. New events appear live.":
    "Eventos enviados por las cámaras y su entrega a los destinos. Los nuevos eventos aparecen en vivo.",
  Camera: "Cámara",
  "All cameras": "Todas las cámaras",
  "Loading events…": "Cargando eventos…",
  "No events yet": "Todavía no hay eventos",
  'Start a camera linked to a target and press "Line crossing" in Cameras.':
    "Inicia una cámara vinculada a un destino y pulsa «Cruce de línea» en Cámaras.",
  "Showing the latest {n} events.": "Mostrando los últimos {n} eventos.",
  Time: "Hora",
  Type: "Tipo",
  Direction: "Dirección",
  Delivery: "Entrega",
  Latency: "Latencia",
  Error: "Error",
  Details: "Detalles",
  Deliveries: "Entregas",
  "Delivery in progress…": "Entrega en curso…",
  "The camera has no target for this event.": "La cámara no tiene destino para este evento.",
  Event: "Evento",
  "attempt {n} · {time} · {ms} ms": "intento {n} · {time} · {ms} ms",
  "attempt {n} · {time} · {ms} ms · HTTP {http}": "intento {n} · {time} · {ms} ms · HTTP {http}",
  Delivered: "Entregado",
  Failed: "Fallida",
  Pending: "Pendiente",
  "No target": "Sin destino",

  // State and level badges
  Running: "Activa",
  Degraded: "Degradada",
  Provisioning: "Aprovisionando",
  Starting: "Iniciando",
  Restarting: "Reiniciando",
  Stopping: "Deteniendo",
  Stopped: "Detenida",
  Draft: "Borrador",
  Documented: "Documentado",
  Captured: "Capturado",
  Verified: "Verificado",

  // Profiles page
  "Camera models: what each one serves and how. A profile imported by hand starts as a draft.":
    "Modelos de cámara: qué sirve cada uno y cómo. Un perfil importado a mano empieza como borrador.",
  "Import profile": "Importar perfil",
  "Validating…": "Validando…",
  "Imported {id}": "Importado {id}",
  "{id} was already installed": "{id} ya estaba instalado",
  "Profile {name} ready": "Perfil {name} listo",
  "Loading profiles…": "Cargando perfiles…",
  "No profiles installed": "No hay perfiles instalados",
  "Import a profile.yaml or a .mvpkg package, for example": "Importa un profile.yaml o un paquete .mvpkg, por ejemplo",
  Firmware: "Firmware",
  Level: "Nivel",
  Signature: "Firma",
  Imported: "Importado",
  Archived: "Archivado",
  Archive: "Archivar",
  Unarchive: "Desarchivar",
  "No problems found.": "No se encontraron problemas.",

  // Assets page
  "Pictures the cameras stream. Each one is encoded once per resolution and looped, so a camera costs almost no CPU.":
    "Imágenes que transmiten las cámaras. Cada una se codifica una vez por resolución y se reproduce en bucle, así una cámara casi no consume CPU.",
  "Upload image": "Subir imagen",
  "Uploading…": "Subiendo…",
  "{file} uploaded": "{file} subida",
  "Loading assets…": "Cargando imágenes…",
  "No assets": "No hay imágenes",
  "Upload a JPEG or PNG taken from the scene the camera should show.":
    "Sube un JPEG o PNG de la escena que la cámara debe mostrar.",
  "built-in": "integrada",
  "{n} camera": "{n} cámara",
  "{n} cameras": "{n} cámaras",
  "Delete {file}?": "¿Eliminar {file}?",

  // Targets page and dialog
  "Receivers of the camera events: a VMS, an NVR or any HTTP endpoint. Link them to cameras when creating them.":
    "Receptores de los eventos de las cámaras: un VMS, un NVR o cualquier endpoint HTTP. Vincúlalos a las cámaras al crearlas.",
  "New target": "Nuevo destino",
  "Loading targets…": "Cargando destinos…",
  "No targets": "No hay destinos",
  "Add the URL where the cameras should send their events.":
    "Agrega la URL a donde las cámaras deben enviar sus eventos.",
  Request: "Solicitud",
  Auth: "Autenticación",
  "Basic ({user})": "Basic ({user})",
  Test: "Probar",
  "Testing…": "Probando…",
  "Send a test request from the node": "Envía una solicitud de prueba desde el nodo",
  "{name}: HTTP {status} in {ms} ms": "{name}: HTTP {status} en {ms} ms",
  "{name}: {error} ({ms} ms)": "{name}: {error} ({ms} ms)",
  failed: "falló",
  "In use by cameras": "En uso por cámaras",
  "Delete target {name}?": "¿Eliminar el destino {name}?",
  "Cameras deliver their events here with the payload of their profile.":
    "Las cámaras entregan aquí sus eventos con el payload de su perfil.",
  "Creating…": "Creando…",
  "Create target": "Crear destino",
  Method: "Método",
  "Stored encrypted; Basic authentication.": "Se guarda cifrada; autenticación Basic.",
  Headers: "Cabeceras",
  "One per line: Name: value": "Una por línea: Nombre: valor",
  "Target {name} created": "Destino {name} creado",

  // New camera dialog
  "The camera appears on the LAN with its own IP and MAC and behaves as its profile describes.":
    "La cámara aparece en la LAN con su propia IP y MAC y se comporta según describe su perfil.",
  "Create camera": "Crear cámara",
  "No profiles installed yet.": "Todavía no hay perfiles instalados.",
  "Import a profile": "Importa un perfil",
  "first, for example": "primero, por ejemplo",
  "IP address": "Dirección IP",
  Netmask: "Máscara de red",
  Gateway: "Puerta de enlace",
  "Factory IP: {ip}": "IP de fábrica: {ip}",
  "Empty: the node's gateway when it is in the subnet.": "Vacío: la puerta de enlace del nodo cuando está en la subred.",
  "Local mode: the camera answers on 127.0.0.1 with its own ports; no IP or MAC on the LAN.":
    "Modo local: la cámara responde en 127.0.0.1 con sus propios puertos; sin IP ni MAC en la LAN.",
  Resolution: "Resolución",
  " (default)": " (por defecto)",
  Image: "Imagen",
  "The stream loops this picture, encoded once.": "El stream reproduce esta imagen en bucle, codificada una vez.",
  "Test pattern": "Patrón de prueba",
  "Camera user": "Usuario de la cámara",
  "Empty: the profile's factory account.": "Vacío: la cuenta de fábrica del perfil.",
  "Event targets": "Destinos de eventos",
  "No targets yet; add them in Targets.": "Todavía no hay destinos; agrégalos en Destinos.",
  "Start with the node (autostart)": "Iniciar con el nodo (autostart)",
  "Start now": "Iniciar ahora",
  "Camera {name} created; starting": "Cámara {name} creada; iniciando",
  "Camera {name} created": "Cámara {name} creada",

  // Camera detail: sections and shell
  General: "General",
  Network: "Red",
  Protocols: "Protocolos",
  Media: "Medios",
  Users: "Usuarios",
  Configuration: "Configuración",
  "Loading camera…": "Cargando cámara…",
  "Camera not found": "Cámara no encontrada",
  "It may have been deleted.": "Puede que se haya eliminado.",
  "Back to the cameras": "Volver a las cámaras",
  "Cannot reach the node: {msg}": "No se puede contactar al nodo: {msg}",
  "Page not found: {path}": "Página no encontrada: {path}",
  "Camera sections": "Secciones de la cámara",
  "Camera {name} deleted": "Cámara {name} eliminada",
  "No protocol is enabled.": "Ningún protocolo está habilitado.",
  network: "la red",
  protocols: "los protocolos",
  " and ": " y ",
  "Saved changes to the {what} apply when the camera restarts (RN-09); it keeps running with the previous ones.":
    "Los cambios guardados en {what} se aplican cuando la cámara se reinicia (RN-09); sigue funcionando con los anteriores.",
  "Restart now": "Reiniciar ahora",
  "No events from this camera yet": "Esta cámara todavía no tiene eventos",
  'Press "Line crossing" while the camera runs.': "Pulsa «Cruce de línea» mientras la cámara funciona.",

  // Camera detail: shared parts
  "Snapshot of {name}": "Instantánea de {name}",
  "Encoding failed: {err}": "Falló la codificación: {err}",
  "Encoding the stream…": "Codificando el stream…",
  "Copy URL": "Copiar URL",

  // General tab
  Tags: "Etiquetas",
  "Comma separated, for filtering.": "Separadas por comas, para filtrar.",
  "Changes reach a running camera at once.": "Los cambios llegan de inmediato a una cámara en marcha.",
  "No targets yet; add them in": "Todavía no hay destinos; agrégalos en",
  " (disabled)": " (deshabilitado)",
  "Camera saved": "Cámara guardada",
  Endpoints: "Endpoints",
  Accounts: "Cuentas",
  "Digest authentication": "autenticación Digest",
  Status: "Estado",
  Serial: "Serie",
  Stream: "Stream",
  "Up for": "Tiempo activo",
  "Last heartbeat": "Último latido",
  "PID / namespace": "PID / namespace",
  Created: "Creada",
  Updated: "Actualizada",
  Retries: "Reintentos",

  // Media tab
  "Stream {name}": "Stream {name}",
  ready: "listo",
  pending: "pendiente",
  missing: "ausente",
  "Manage images": "Gestionar imágenes",
  "Also changes the profile parameter bound to it.": "También cambia el parámetro del perfil vinculado.",
  "Fixed by the profile.": "Fijada por el perfil.",
  "Frame rate (fps)": "Cuadros por segundo (fps)",
  "Between {min} and {max}.": "Entre {min} y {max}.",
  Codec: "Códec",
  Bitrate: "Bitrate",
  "Stream saved; it is encoded again and the camera switches to it":
    "Stream guardado; se codifica de nuevo y la cámara cambia a él",
  "Applied without restarting: the picture is encoded once and the camera switches to it (RN-09).":
    "Se aplica sin reiniciar: la imagen se codifica una vez y la cámara cambia a ella (RN-09).",
  Snapshot: "Instantánea",
  "The camera has no stream.": "La cámara no tiene stream.",

  // Network tab
  "Local mode: the camera answers on 127.0.0.1 with its own ports, so only the MAC and the DNS servers apply here.":
    "Modo local: la cámara responde en 127.0.0.1 con sus propios puertos, así que aquí solo aplican la MAC y los servidores DNS.",
  "Empty: the parent interface's.": "Vacío: la de la interfaz padre.",
  "Empty: the node's, when it is in the subnet.": "Vacío: la del nodo, cuando está en la subred.",
  "MAC address": "Dirección MAC",
  "Derived again from the camera ID on save.": "Se deriva de nuevo del ID de la cámara al guardar.",
  "Locally administered and stable.": "Administrada localmente y estable.",
  "Go back to the MAC derived from the camera ID": "Volver a la MAC derivada del ID de la cámara",
  "DNS servers": "Servidores DNS",
  "Up to 3; empty: the node's.": "Hasta 3; vacío: los del nodo.",
  "Parent interface": "Interfaz padre",
  "Where the camera's macvlan attaches.": "Donde se conecta el macvlan de la cámara.",
  "Network saved; it applies when the camera restarts": "Red guardada; se aplica cuando la cámara se reinicia",
  "Network saved": "Red guardada",
  "Network changes apply when the camera restarts (RN-09). The new address is probed before it is used.":
    "Los cambios de red se aplican cuando la cámara se reinicia (RN-09). La nueva dirección se sondea antes de usarla.",

  // Protocols tab
  Protocol: "Protocolo",
  Engine: "Motor",
  "Profile default": "Por defecto del perfil",
  "serves clients": "atiende clientes",
  "sends out": "envía",
  "Port of {name}": "Puerto de {name}",
  "Protocols saved; they apply when the camera restarts": "Protocolos guardados; se aplican cuando la cámara se reinicia",
  "Protocols saved": "Protocolos guardados",
  "The protocols come from the profile (RN-04). Changes apply when the camera restarts.":
    "Los protocolos vienen del perfil (RN-04). Los cambios se aplican cuando la cámara se reinicia.",

  // Users tab
  "unchanged": "sin cambios",
  required: "requerida",
  "Role of {name}": "Rol de {name}",
  "Password of {name}": "Contraseña de {name}",
  "the new account": "la cuenta nueva",
  "Remove {name}": "Quitar {name}",
  "Add account": "Agregar cuenta",
  "Cameras accept weak passwords on purpose: they imitate real devices.":
    "Las cámaras aceptan contraseñas débiles a propósito: imitan dispositivos reales.",
  "Accounts saved; the camera uses them at once": "Cuentas guardadas; la cámara las usa de inmediato",
  "At least one admin (RN-11). Changes apply at once; clients re-authenticate with the new passwords.":
    "Al menos un administrador (RN-11). Los cambios se aplican de inmediato; los clientes se reautentican con las nuevas contraseñas.",

  // Configuration tab
  Parameter: "Parámetro",
  Value: "Valor",
  Effect: "Efecto",
  "Last change": "Último cambio",
  "profile default": "por defecto del perfil",
  "client {id}": "cliente {id}",
  effective: "efectivo",
  declarative: "declarativo",
  "Bound to {bind}": "Vinculado a {bind}",
  "Stored and returned; no effect on the simulation": "Se guarda y se devuelve; sin efecto en la simulación",
  "Loading parameters…": "Cargando parámetros…",
  "The profile declares no parameters": "El perfil no declara parámetros",
  "{n} parameter saved": "{n} parámetro guardado",
  "{n} parameters saved": "{n} parámetros guardados",
  "Value of {key}": "Valor de {key}",
  On: "Activado",
  Off: "Desactivado",
  "Applied at once. The last change wins, from the panel or a client of the emulated API (RN-08).":
    "Se aplica de inmediato. Gana el último cambio, ya sea del panel o de un cliente de la API emulada (RN-08).",

  // Clone / restore dialogs
  "Same profile, parameters, accounts, protocols, picture and targets; its own ID, serial and MAC.":
    "Mismo perfil, parámetros, cuentas, protocolos, imagen y destinos; con su propio ID, serie y MAC.",
  "Cloning…": "Clonando…",
  "Clone camera": "Clonar cámara",
  "Same netmask and gateway as {name}.": "Misma máscara y puerta de enlace que {name}.",
  "Start it now": "Iniciar ahora",
  "Camera {name} created from {src}": "Cámara {name} creada a partir de {src}",
  "Restore {name}": "Restaurar {name}",
  "Like the reset button of the real device. The picture is kept.":
    "Como el botón de reinicio del dispositivo real. La imagen se conserva.",
  "Restore settings": "Restaurar ajustes",
  "Factory reset": "Restablecer de fábrica",
  "Parameters, accounts and protocols go back to the profile's defaults. The network identity stays.":
    "Los parámetros, cuentas y protocolos vuelven a los valores por defecto del perfil. La identidad de red se mantiene.",
  "Everything above, plus the MAC derived from the camera ID.":
    "Todo lo anterior, más la MAC derivada del ID de la cámara.",
  "Everything above, plus the profile's factory address {ip} and the default MAC.":
    "Todo lo anterior, más la dirección de fábrica del perfil {ip} y la MAC por defecto.",
  "Everything above, plus the profile's factory address and the default MAC.":
    "Todo lo anterior, más la dirección de fábrica del perfil y la MAC por defecto.",
  "Restore level": "Nivel de restauración",
  "Restoring…": "Restaurando…",
  "{name} restored; it reboots": "{name} restaurada; se reinicia",
  "{name} restored": "{name} restaurada",
  "The camera is running: it reboots to apply the reset, as the real one does.":
    "La cámara está en marcha: se reinicia para aplicar el restablecimiento, como lo hace la real.",
  "The profile declares no factory address, so a factory reset is refused.":
    "El perfil no declara dirección de fábrica, así que se rechaza el restablecimiento de fábrica.",
  "New address:": "Nueva dirección:",
  "Another camera cannot be using it.": "Ninguna otra cámara puede estar usándola.",

  // Copy button
  Copy: "Copiar",
  Copied: "Copiado",
  "Copied to the clipboard": "Copiado al portapapeles",
  "The browser did not allow copying; select the text and copy it by hand":
    "El navegador no permitió copiar; selecciona el texto y cópialo a mano",

  // Settings page
  "Limits of this node and the network the cameras join.":
    "Límites de este nodo y la red a la que se unen las cámaras.",
  Limits: "Límites",
  "Creating or starting a camera beyond them is rejected with the reason.":
    "Crear o iniciar una cámara más allá de ellos se rechaza indicando el motivo.",
  "Maximum cameras": "Máximo de cámaras",
  "Event retention (days)": "Retención de eventos (días)",
  "Maximum RAM use (%)": "Uso máximo de RAM (%)",
  "Of the node's memory, counting what the new camera needs.":
    "De la memoria del nodo, contando lo que necesita la nueva cámara.",
  "Maximum sustained CPU (%)": "CPU sostenida máxima (%)",
  "One-minute average of the node.": "Promedio de un minuto del nodo.",
  "Not used in local mode.": "No se usa en modo local.",
  "New cameras attach to this interface with macvlan. Empty: the default route's interface.":
    "Las nuevas cámaras se conectan a esta interfaz con macvlan. Vacío: la interfaz de la ruta por defecto.",
  " (down)": " (caída)",
  "Settings saved": "Configuración guardada",
  Node: "Nodo",
  "network namespaces": "network namespaces",
  Hostname: "Hostname",
  Version: "Versión",
  CPUs: "CPUs",
  Memory: "Memoria",
  "Default route": "Ruta por defecto",
  " via {gw}": " vía {gw}",
  "{running} running, {error} in error, {total} total": "{running} activas, {error} con error, {total} en total",
  "Database writes": "Escrituras en base de datos",
  "{tx} transactions · {st} statements": "{tx} transacciones · {st} sentencias",
  "Metrics at": "Métricas a las",
  "Loading…": "Cargando…",
};
