import type { Translate } from "./i18n";

/** What each reason code of a camera's state means. The node sends the
 * code beside its English detail, so the panel explains it in the
 * reader's language. */
const reasons: Record<string, string> = {
  ip_in_use: "Another device on the LAN answers on this camera's IP.",
  mac_in_use: "Another device uses this camera's MAC.",
  no_interface: "The camera's network card is missing or down.",
  parent_busy: "Its network card cannot take this network mode: one card takes macvlan or ipvlan cameras, not both.",
  unsupported: "This node's kernel lacks the camera's network mode.",
  invalid: "The network helper refused the camera's settings.",
  dhcp_waiting: "Waiting for a DHCP lease.",
  dhcp_no_factory: "No DHCP server answered, and the profile has no factory address.",
  dhcp_factory_in_use: "No DHCP server answered, and the factory address is in use.",
  stream: "Its stream could not be encoded.",
  config: "Its configuration is incomplete; edit the camera.",
  launch: "The node could not start it.",
  exited: "The camera process ended.",
  no_hello: "The camera process did not answer.",
  not_ready: "The camera did not become ready in time.",
  config_rejected: "The camera refused its configuration.",
  camera_failed: "The camera could not start its protocols.",
  no_heartbeat: "The camera stopped answering.",
  admission: "The node has no room to start it.",
  lease_restart: "Restarting with its new DHCP lease.",
  lease_lost: "Restarting: its DHCP lease expired.",
};

interface Reason {
  reason_code?: string;
  reason: string;
}

/** The reason of a camera's state in the reader's language, or the node's
 * detail when the code is unknown. */
export function reasonText(status: Reason, t: Translate): string {
  const known = status.reason_code ? reasons[status.reason_code] : undefined;
  return known ? t(known) : status.reason;
}

/** The node's detail when reasonText explained the code: the addresses,
 * names and errors it carries. */
export function reasonDetail(status: Reason): string {
  return status.reason_code && reasons[status.reason_code] ? status.reason : "";
}
