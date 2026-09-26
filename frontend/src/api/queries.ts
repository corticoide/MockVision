import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { navigate } from "@/lib/router";
import {
  api,
  ApiError,
  type Asset,
  type AuditEntry,
  type AuditPage,
  type BulkAction,
  type Camera,
  type CameraUserInput,
  type CloneCamera,
  type CreateCamera,
  type EventPage,
  type ImportResult,
  type Job,
  type JobDetail,
  type JobPage,
  type Settings,
  type TargetInput,
  type TokenInput,
  type UpdateCamera,
  unwrap,
  upload,
} from "./client";

export const keys = {
  me: ["me"] as const,
  node: ["node"] as const,
  nodeMetrics: ["node", "metrics"] as const,
  nodeHistory: ["node", "history"] as const,
  tokens: ["tokens"] as const,
  audit: (f: AuditFilter) => ["audit", f] as const,
  failedEvents: ["events", "failed"] as const,
  jobs: (f: JobFilter) => ["jobs", f] as const,
  job: (id: string) => ["job", id] as const,
  settings: ["settings"] as const,
  cameras: ["cameras"] as const,
  camera: (id: string) => ["cameras", id] as const,
  cameraConfig: (id: string) => ["cameras", id, "config"] as const,
  profiles: ["profiles"] as const,
  profile: (id: string, version: string) => ["profiles", id, version] as const,
  assets: ["assets"] as const,
  targets: ["targets"] as const,
  events: (cameraId?: string) => ["events", cameraId ?? "all"] as const,
};

// --- Session ---

export type MeState =
  | { status: "authenticated"; username: string }
  | { status: "login" }
  | { status: "setup" };

export function useMe() {
  return useQuery({
    queryKey: keys.me,
    queryFn: async (): Promise<MeState> => {
      const res = await api.GET("/auth/me");
      if (res.response.status === 401) {
        const problem = res.error as { setup_required?: boolean } | undefined;
        return problem?.setup_required ? { status: "setup" } : { status: "login" };
      }
      const me = unwrap(res);
      return { status: "authenticated", username: me.user.username };
    },
    staleTime: 60_000,
  });
}

export function useLogin(mode: "login" | "setup") {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: { username: string; password: string }) =>
      unwrap(mode === "setup" ? await api.POST("/auth/setup", { body }) : await api.POST("/auth/login", { body })),
    onSuccess: () => qc.invalidateQueries(),
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await api.POST("/auth/logout");
    },
    // Even if the request fails the session is unusable: go back to the
    // login and drop everything cached for the previous session. The me
    // query is set, not cleared, so the mounted app sees the change.
    onSettled: () => {
      qc.cancelQueries();
      qc.setQueryData<MeState>(keys.me, { status: "login" });
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] });
      navigate("/");
    },
  });
}

// --- Node ---

export function useNode() {
  return useQuery({ queryKey: keys.node, queryFn: async () => unwrap(await api.GET("/node")), staleTime: 30_000 });
}

export function useNodeMetrics() {
  // Kept fresh by the node topic of the WebSocket; the query is the fallback.
  return useQuery({
    queryKey: keys.nodeMetrics,
    queryFn: async () => unwrap(await api.GET("/node/metrics")),
    refetchInterval: 30_000,
  });
}

/** Node samples of the last ten minutes; the node topic appends new ones. */
export function useNodeHistory() {
  return useQuery({
    queryKey: keys.nodeHistory,
    queryFn: async () => unwrap(await api.GET("/node/history")).samples,
    staleTime: Infinity,
  });
}

export function useSettings() {
  return useQuery({ queryKey: keys.settings, queryFn: async () => unwrap(await api.GET("/settings")) });
}

export function useUpdateSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: Partial<Settings>) => unwrap(await api.PATCH("/settings", { body })),
    onSuccess: (data) => qc.setQueryData(keys.settings, data),
  });
}

// --- Cameras ---

export function useCameras() {
  return useQuery({
    queryKey: keys.cameras,
    queryFn: async () => unwrap(await api.GET("/cameras")).items,
  });
}

/** Applies one action to several cameras; each camera reports its own result. */
export function useBulkCameras() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: BulkAction) => unwrap(await api.POST("/cameras/actions/bulk", { body })),
    onSuccess: (res) => {
      const deleted = new Set<string>();
      for (const r of res.results) {
        if (!r.ok) continue;
        if (r.camera) patchCameraCache(qc, r.camera);
        else if (res.action === "delete") deleted.add(r.id);
      }
      if (deleted.size) qc.setQueryData<Camera[]>(keys.cameras, (list) => list?.filter((c) => !deleted.has(c.id)));
    },
    onError: () => qc.invalidateQueries({ queryKey: keys.cameras }),
  });
}

export function useCameraConfig(id: string) {
  return useQuery({
    queryKey: keys.cameraConfig(id),
    queryFn: async () => unwrap(await api.GET("/cameras/{id}/config", { params: { path: { id } } })).params,
  });
}

/** Replaces one camera in the list cache. */
export function patchCameraCache(qc: ReturnType<typeof useQueryClient>, camera: Camera) {
  qc.setQueryData<Camera[]>(keys.cameras, (list) => {
    if (!list) return list;
    const i = list.findIndex((c) => c.id === camera.id);
    if (i < 0) return [...list, camera].sort((a, b) => a.name.localeCompare(b.name));
    const next = list.slice();
    next[i] = camera;
    return next;
  });
}

export function useCreateCamera() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: CreateCamera) => unwrap(await api.POST("/cameras", { body })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
  });
}

export function useCameraAction() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; action: "start" | "stop" | "restart" }) =>
      unwrap(await api.POST("/cameras/{id}/actions/{action}", { params: { path: v } })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
    onError: () => qc.invalidateQueries({ queryKey: keys.cameras }),
  });
}

export function useDeleteCamera() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      unwrap(await api.DELETE("/cameras/{id}", { params: { path: { id } } }));
      return id;
    },
    onSuccess: (id) => qc.setQueryData<Camera[]>(keys.cameras, (list) => list?.filter((c) => c.id !== id)),
  });
}

/** One camera, from the live list: the WebSocket keeps it current. */
export function useCamera(id: string) {
  const list = useCameras();
  return { ...list, data: list.data?.find((c) => c.id === id) };
}

export function useUpdateCamera() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; body: UpdateCamera }) =>
      unwrap(await api.PATCH("/cameras/{id}", { params: { path: { id: v.id } }, body: v.body })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
  });
}

export function useSetCameraUsers(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (users: CameraUserInput[]) =>
      unwrap(await api.PUT("/cameras/{id}/users", { params: { path: { id } }, body: { users } })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
  });
}

export function useSetCameraProtocols(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (protocols: { instance: string; enabled?: boolean; port?: number }[]) =>
      unwrap(await api.PUT("/cameras/{id}/protocols", { params: { path: { id } }, body: { protocols } })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
  });
}

export function useUpdateStream(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { stream: string; asset_id?: string; resolution?: string; fps?: number }) => {
      const { stream, ...body } = v;
      return unwrap(await api.PATCH("/cameras/{id}/streams/{stream}", { params: { path: { id, stream } }, body }));
    },
    onSuccess: (camera) => {
      patchCameraCache(qc, camera);
      qc.invalidateQueries({ queryKey: keys.cameraConfig(id) });
    },
  });
}

export function useResetCamera(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (scope: "settings" | "full") =>
      unwrap(await api.POST("/cameras/{id}/actions/factory-reset", { params: { path: { id } }, body: { scope } })),
    onSuccess: (camera) => {
      patchCameraCache(qc, camera);
      qc.invalidateQueries({ queryKey: keys.cameraConfig(id) });
    },
  });
}

export function useCloneCamera(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: CloneCamera) =>
      unwrap(await api.POST("/cameras/{id}/actions/clone", { params: { path: { id } }, body })),
    onSuccess: (camera) => patchCameraCache(qc, camera),
  });
}

export function usePatchConfig(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (values: Record<string, unknown>) =>
      unwrap(await api.PATCH("/cameras/{id}/config", { params: { path: { id } }, body: { values } })).params,
    onSuccess: (params) => qc.setQueryData(keys.cameraConfig(id), params),
  });
}

export function useTrigger() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; type: string; direction?: "A->B" | "B->A" | "none" }) =>
      unwrap(
        await api.POST("/cameras/{id}/events", {
          params: { path: { id: v.id } },
          body: { type: v.type, direction: v.direction },
        }),
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["events"] }),
  });
}

// --- Profiles ---

export function useProfiles() {
  return useQuery({ queryKey: keys.profiles, queryFn: async () => unwrap(await api.GET("/profiles")).items });
}

export function useProfile(ref: { id: string; version: string } | null) {
  return useQuery({
    queryKey: ref ? keys.profile(ref.id, ref.version) : ["profiles", "none"],
    enabled: !!ref,
    staleTime: Infinity, // versions are immutable
    queryFn: async () => {
      const [vendor, model] = ref!.id.split("/");
      return unwrap(
        await api.GET("/profiles/{vendor}/{model}/versions/{version}", {
          params: { path: { vendor, model, version: ref!.version } },
        }),
      );
    },
  });
}

export function useProfileAction() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; version: string; action: "archive" | "unarchive" }) => {
      const [vendor, model] = v.id.split("/");
      return unwrap(
        await api.POST("/profiles/{vendor}/{model}/versions/{version}/actions/{action}", {
          params: { path: { vendor, model, version: v.version, action: v.action } },
        }),
      );
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.profiles }),
  });
}

/** An import's answer: the result, or the job when it goes on in the background (202). */
export type ImportAnswer = ImportResult | { job: Job };

export function useImportPackage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => upload<ImportAnswer>("/packages", file),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.profiles }),
  });
}

// --- Assets ---

export function useAssets() {
  return useQuery({ queryKey: keys.assets, queryFn: async () => unwrap(await api.GET("/assets")).items });
}

export function useUploadAsset() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => upload<Asset>("/assets", file),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.assets }),
  });
}

export function useDeleteAsset() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => unwrap(await api.DELETE("/assets/{id}", { params: { path: { id } } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.assets }),
  });
}

// --- Targets ---

export function useTargets() {
  return useQuery({ queryKey: keys.targets, queryFn: async () => unwrap(await api.GET("/targets")).items });
}

export function useCreateTarget() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: TargetInput) => unwrap(await api.POST("/targets", { body })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.targets }),
  });
}

export function useUpdateTarget() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; body: TargetInput }) =>
      unwrap(await api.PATCH("/targets/{id}", { params: { path: { id: v.id } }, body: v.body })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.targets }),
  });
}

export function useDeleteTarget() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => unwrap(await api.DELETE("/targets/{id}", { params: { path: { id } } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.targets }),
  });
}

export function useTestTarget() {
  return useMutation({
    mutationFn: async (id: string) =>
      unwrap(await api.POST("/targets/{id}/actions/test", { params: { path: { id } } })),
  });
}

// --- Events ---

export function useEvents(cameraId?: string) {
  return useQuery({
    queryKey: keys.events(cameraId),
    queryFn: async (): Promise<EventPage> =>
      unwrap(await api.GET("/events", { params: { query: { camera_id: cameraId, limit: 100 } } })),
  });
}

/** The latest events a target gave up on, for the dashboard. */
export function useFailedEvents() {
  return useQuery({
    queryKey: keys.failedEvents,
    queryFn: async (): Promise<EventPage> =>
      unwrap(await api.GET("/events", { params: { query: { delivery: "failed", limit: 5 } } })),
  });
}

// --- API tokens ---

export function useTokens() {
  return useQuery({ queryKey: keys.tokens, queryFn: async () => unwrap(await api.GET("/tokens")).items });
}

export function useCreateToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: TokenInput) => unwrap(await api.POST("/tokens", { body })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.tokens }),
  });
}

export function useRevokeToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => unwrap(await api.DELETE("/tokens/{id}", { params: { path: { id } } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.tokens }),
  });
}

// --- Audit ---

/** Filters of the audit log; empty fields match everything. */
export interface AuditFilter {
  origin?: "panel" | "api" | "camera" | "system";
  entity_type?: string;
  entity_id?: string;
  token_id?: string;
  action?: string;
}

/** Whether an entry passes a filter, as the node would decide it. */
export function auditMatches(f: AuditFilter, e: AuditEntry): boolean {
  if (f.origin && e.origin !== f.origin) return false;
  if (f.entity_type && e.entity.type !== f.entity_type) return false;
  if (f.entity_id && e.entity.id !== f.entity_id) return false;
  if (f.token_id && e.token?.id !== f.token_id) return false;
  if (f.action && e.action !== f.action && !e.action.startsWith(`${f.action}.`)) return false;
  return true;
}

/** Audit entries, newest first, a page at a time; new ones arrive live. */
export function useAudit(filter: AuditFilter) {
  return useInfiniteQuery({
    queryKey: keys.audit(filter),
    initialPageParam: "",
    queryFn: async ({ pageParam }): Promise<AuditPage> =>
      unwrap(await api.GET("/audit", { params: { query: { ...filter, cursor: pageParam || undefined, limit: 50 } } })),
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
}

// --- Jobs ---

/** Filters of the job list. Status is a status, "active" or "finished". */
export interface JobFilter {
  status?: string;
  type?: Job["type"];
}

/** Whether a job belongs to a filtered list, as the node decides it. */
export function jobMatches(f: JobFilter, j: Job): boolean {
  if (f.type && j.type !== f.type) return false;
  switch (f.status) {
    case undefined:
    case "":
      return true;
    case "active":
      return ["queued", "running", "waiting", "interrupted"].includes(j.status);
    case "finished":
      return ["completed", "failed", "canceled"].includes(j.status);
  }
  return j.status === f.status;
}

/** Jobs, newest first; the jobs topic keeps them current. */
export function useJobs(filter: JobFilter, limit = 100) {
  return useQuery({
    queryKey: keys.jobs(filter),
    queryFn: async (): Promise<JobPage> =>
      unwrap(
        await api.GET("/jobs", {
          params: { query: { status: filter.status || undefined, type: filter.type, limit } },
        }),
      ),
  });
}

/** A job with its history; its topic appends new events. */
export function useJob(id: string | null) {
  return useQuery({
    queryKey: keys.job(id ?? "none"),
    enabled: !!id,
    queryFn: async (): Promise<JobDetail> => unwrap(await api.GET("/jobs/{id}", { params: { path: { id: id! } } })),
  });
}

export function useCreateJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (body: { type: "renditions.prepare"; params?: { asset_id?: string } }) =>
      unwrap(await api.POST("/jobs", { body })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["jobs"] }),
  });
}

export function useJobAction() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; action: "cancel" | "resume" | "answer"; answer?: string }) =>
      unwrap(
        await api.POST("/jobs/{id}/actions/{action}", {
          params: { path: { id: v.id, action: v.action } },
          body: v.action === "answer" ? { answer: v.answer } : undefined,
        }),
      ),
    onSuccess: (job) => {
      qc.invalidateQueries({ queryKey: keys.job(job.id) });
      qc.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

export function isUnauthenticated(err: unknown) {
  return err instanceof ApiError && err.status === 401;
}
