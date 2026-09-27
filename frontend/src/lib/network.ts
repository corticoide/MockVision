import type { Camera } from "@/api/client";

/** Where a camera answers: the address it holds, else its static one; a
 * DHCP camera without a lease yet shows "DHCP". */
export function cameraIP(c: Camera) {
  if (c.status.ip) return c.status.ip;
  if (c.network.ip) return c.network.ip;
  return c.network.ip_mode === "dhcp" ? "DHCP" : "127.0.0.1";
}
