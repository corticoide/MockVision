import { Activity } from "lucide-react";
import { useState } from "react";
import { errorMessage } from "@/api/client";
import { useCameras, useEvents } from "@/api/queries";
import { EventTable } from "@/components/EventTable";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Select } from "@/components/ui/form";
import { useT } from "@/lib/i18n";

export function EventsPage() {
  const [cameraId, setCameraId] = useState("");
  const { data: cameras } = useCameras();
  const { data, isLoading, error } = useEvents(cameraId || undefined);
  const events = data?.items ?? [];
  const t = useT();

  return (
    <>
      <PageHeader
        title={t("Events")}
        description={t("Events sent by the cameras and the delivery to their targets. New events appear live.")}
        actions={
          <Select value={cameraId} onChange={(e) => setCameraId(e.target.value)} className="w-56" aria-label={t("Camera")}>
            <option value="">{t("All cameras")}</option>
            {cameras?.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </Select>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title={t("Loading events…")} />
        ) : events.length === 0 ? (
          <Empty icon={<Activity />} title={t("No events yet")}>
            {t('Start a camera linked to a target and press "Line crossing" in Cameras.')}
          </Empty>
        ) : (
          <EventTable events={events} />
        )}
      </Card>
      {data?.next_cursor && <p className="mt-2 text-xs text-muted">{t("Showing the latest {n} events.", { n: events.length })}</p>}
    </>
  );
}
