import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { navigate } from "@/lib/router";
import {
  api,
  ApiError,
  type Asset,
  type Camera,
  type CreateCamera,
  type EventPage,
  type ImportResult,
  type Settings,
  type TargetInput,
  unwrap,
  upload,
} from "./client";

export const keys = {
  me: ["me"] as const,
  node: ["node"] as const,
  nodeMetrics: ["node", "metrics"] as const,
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

export function useUpdateCamera() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { id: string; body: { name?: string; autostart?: boolean; target_ids?: string[] } }) =>
      unwrap(await api.PATCH("/cameras/{id}", { params: { path: { id: v.id } }, body: v.body })),
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

export function useImportPackage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (file: File) => upload<ImportResult>("/packages", file),
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

export function isUnauthenticated(err: unknown) {
  return err instanceof ApiError && err.status === 401;
}
