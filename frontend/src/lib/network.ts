import type { Camera, NodeInfo } from "@/api/client";

/** Where a camera answers: the address it holds, else its static one; a
 * DHCP camera without a lease yet shows "DHCP". */
export function cameraIP(c: Camera) {
  if (c.status.ip) return c.status.ip;
  if (c.network.ip) return c.network.ip;
  return c.network.ip_mode === "dhcp" ? "DHCP" : "127.0.0.1";
}

/** The network card cameras attach to when they name none. */
export function defaultParent(node: NodeInfo | undefined) {
  return node?.parent_interface || node?.default_interface || "";
}

/** Whether the node sees a network card as Wi-Fi, where access points
 * refuse the extra MACs of macvlan cameras. */
export function isWireless(node: NodeInfo | undefined, name: string) {
  return node?.interfaces?.find((i) => i.name === name)?.wireless ?? false;
}
