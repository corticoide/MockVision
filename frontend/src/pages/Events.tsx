import { Activity } from "lucide-react";
import { useState } from "react";
import { errorMessage } from "@/api/client";
import { useCameras, useEvents } from "@/api/queries";
import { EventTable } from "@/components/EventTable";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Select } from "@/components/ui/form";

export function EventsPage() {
  const [cameraId, setCameraId] = useState("");
  const { data: cameras } = useCameras();
  const { data, isLoading, error } = useEvents(cameraId || undefined);
  const events = data?.items ?? [];

  return (
    <>
      <PageHeader
        title="Events"
        description="Events sent by the cameras and the delivery to their targets. New events appear live."
        actions={
          <Select value={cameraId} onChange={(e) => setCameraId(e.target.value)} className="w-56" aria-label="Camera">
            <option value="">All cameras</option>
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
          <Empty title="Loading events…" />
        ) : events.length === 0 ? (
          <Empty icon={<Activity />} title="No events yet">
            Start a camera linked to a target and press “Line crossing” in Cameras.
          </Empty>
        ) : (
          <EventTable events={events} />
        )}
      </Card>
      {data?.next_cursor && <p className="mt-2 text-xs text-muted">Showing the latest {events.length} events.</p>}
    </>
  );
}
