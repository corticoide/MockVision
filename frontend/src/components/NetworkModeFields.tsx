import { Field, Select } from "@/components/ui/form";
import { useT } from "@/lib/i18n";

export interface NetworkModes {
  mode: string;
  ipMode: string;
}

/** How a camera joins the LAN: its network mode and its addressing, as the
 * new camera dialog and the Network tab ask them. ipvlan cameras share the
 * node's MAC, so they take a static IP. */
export function NetworkModeFields({
  value,
  onChange,
  factoryIP,
  errors,
  disabled,
}: {
  value: NetworkModes;
  onChange: (next: NetworkModes) => void;
  factoryIP?: string;
  errors?: Record<string, string>;
  disabled?: boolean;
}) {
  const t = useT();
  const ipvlan = value.mode === "ipvlan";
  const dhcp = value.ipMode === "dhcp" && !ipvlan;
  let addressing = t("A fixed address, probed on the LAN before use.");
  if (dhcp) {
    addressing = factoryIP
      ? t("It asks the LAN's DHCP server; if none answers within about 15 s it takes the factory address, {ip}.", { ip: factoryIP })
      : t("It asks the LAN's DHCP server.");
  } else if (ipvlan) {
    addressing = t("ipvlan cameras share the node's MAC, so they need a static IP.");
  }
  return (
    <>
      <Field
        label={t("Network mode")}
        error={errors?.["network.mode"]}
        hint={
          ipvlan ? t("The node's MAC: for Wi-Fi and switch ports that allow one MAC. No DHCP.") : t("Its own MAC, like a real device: for wired networks.")
        }
      >
        <Select
          value={value.mode}
          onChange={(e) => onChange({ mode: e.target.value, ipMode: e.target.value === "ipvlan" ? "static" : value.ipMode })}
          disabled={disabled}
        >
          <option value="macvlan">{t("macvlan — its own MAC (wired)")}</option>
          <option value="ipvlan">{t("ipvlan — the node's MAC (Wi-Fi)")}</option>
        </Select>
      </Field>
      <Field label={t("Addressing")} error={errors?.["network.ip_mode"]} hint={addressing}>
        <Select value={dhcp ? "dhcp" : "static"} onChange={(e) => onChange({ ...value, ipMode: e.target.value })} disabled={disabled}>
          <option value="static">{t("Static IP")}</option>
          <option value="dhcp" disabled={ipvlan}>
            {t("DHCP")}
          </option>
        </Select>
      </Field>
    </>
  );
}
