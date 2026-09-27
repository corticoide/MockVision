import { Image as ImageIcon, Trash2, Upload } from "lucide-react";
import { useRef } from "react";
import { errorMessage } from "@/api/client";
import { useAssets, useDeleteAsset, useUploadAsset } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { useT } from "@/lib/i18n";
import { formatBytes } from "@/lib/utils";

export function AssetsPage() {
  const { data: assets, isLoading, error } = useAssets();
  const uploader = useUploadAsset();
  const del = useDeleteAsset();
  const input = useRef<HTMLInputElement>(null);
  const t = useT();

  const onFile = (file: File | undefined) => {
    if (!file) return;
    uploader.mutate(file, {
      onSuccess: (a) => toast(t("{file} uploaded", { file: a.filename }), "ok"),
      onError: (err) => toast(errorMessage(err), "error"),
    });
    if (input.current) input.current.value = "";
  };

  return (
    <>
      <PageHeader
        title={t("Assets")}
        description={t("Pictures the cameras stream. Each one is encoded once per resolution and looped, so a camera costs almost no CPU.")}
        actions={
          <>
            <input ref={input} type="file" accept="image/jpeg,image/png" className="hidden" onChange={(e) => onFile(e.target.files?.[0])} />
            <Button variant="primary" onClick={() => input.current?.click()} disabled={uploader.isPending}>
              <Upload /> {uploader.isPending ? t("Uploading…") : t("Upload image")}
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {isLoading ? (
        <Card>
          <Empty title={t("Loading assets…")} />
        </Card>
      ) : !assets?.length ? (
        <Card>
          <Empty icon={<ImageIcon />} title={t("No assets")}>
            {t("Upload a JPEG or PNG taken from the scene the camera should show.")}
          </Empty>
        </Card>
      ) : (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(240px,1fr))] gap-3">
          {assets.map((a) => (
            <Card key={a.id} className="overflow-hidden">
              <img
                src={`/api/v1/assets/${a.id}/content`}
                alt={a.filename}
                loading="lazy"
                className="aspect-video w-full border-b border-border bg-black object-contain"
              />
              <div className="flex items-start justify-between gap-2 px-3 py-2">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="truncate font-medium" title={a.filename}>
                      {a.filename}
                    </span>
                    {a.builtin && <Badge tone="info">{t("built-in")}</Badge>}
                  </div>
                  <Mono className="text-muted">
                    {a.width}×{a.height} · {formatBytes(a.size)} · {t(a.camera_count === 1 ? "{n} camera" : "{n} cameras", { n: a.camera_count })}
                  </Mono>
                </div>
                {!a.builtin && (
                  <Button
                    size="icon"
                    variant="ghost"
                    title={t("Delete")}
                    aria-label={t("Delete")}
                    disabled={del.isPending || a.camera_count > 0}
                    onClick={() => {
                      if (confirm(t("Delete {file}?", { file: a.filename }))) del.mutate(a.id, { onError: (err) => toast(errorMessage(err), "error") });
                    }}
                  >
                    <Trash2 />
                  </Button>
                )}
              </div>
            </Card>
          ))}
        </div>
      )}
    </>
  );
}
