import { useQueryClient } from "@tanstack/react-query";
import { type ComponentType, lazy, Suspense, useEffect } from "react";
import { errorMessage } from "@/api/client";
import { startLive, stopLive } from "@/api/live";
import { useMe } from "@/api/queries";
import { Layout, Logo } from "@/components/Layout";
import { Toaster } from "@/components/toast";
import { Empty, Notice } from "@/components/ui/card";
import { useT } from "@/lib/i18n";
import { usePath } from "@/lib/router";
import { AssetsPage } from "@/pages/Assets";
import { AuditPage } from "@/pages/Audit";
import { CamerasPage } from "@/pages/Cameras";
import { DashboardPage } from "@/pages/Dashboard";
import { EventsPage } from "@/pages/Events";
import { JobsPage } from "@/pages/Jobs";
import { LoginPage } from "@/pages/Login";
import { PluginsPage } from "@/pages/Plugins";
import { ProfilePage } from "@/pages/ProfilePage";
import { ProfilesPage } from "@/pages/Profiles";
import { SettingsPage } from "@/pages/Settings";
import { TargetsPage } from "@/pages/Targets";

// The camera page and its editors load the first time a camera is opened.
const CameraPage = lazy(() => import("@/pages/camera/CameraPage").then((m) => ({ default: m.CameraPage })));

const pages: Record<string, ComponentType> = {
  "/": DashboardPage,
  "/cameras": CamerasPage,
  "/events": EventsPage,
  "/profiles": ProfilesPage,
  "/plugins": PluginsPage,
  "/assets": AssetsPage,
  "/targets": TargetsPage,
  "/jobs": JobsPage,
  "/audit": AuditPage,
  "/settings": SettingsPage,
};

export function App() {
  const qc = useQueryClient();
  const me = useMe();
  const t = useT();
  const authenticated = me.data?.status === "authenticated";

  // The WebSocket lives as long as the session.
  useEffect(() => {
    if (!authenticated) return;
    startLive(qc);
    return () => stopLive();
  }, [authenticated, qc]);

  let content;
  if (me.isLoading) {
    content = (
      <div className="flex h-full items-center justify-center">
        <Logo />
      </div>
    );
  } else if (me.error) {
    content = (
      <div className="flex h-full items-center justify-center p-6">
        <Notice tone="error">{t("Cannot reach the node: {msg}", { msg: errorMessage(me.error) })}</Notice>
      </div>
    );
  } else if (me.data?.status === "authenticated") {
    content = (
      <Layout username={me.data.username}>
        <Routes />
      </Layout>
    );
  } else {
    content = <LoginPage mode={me.data?.status ?? "login"} />;
  }

  return (
    <>
      {content}
      <Toaster />
    </>
  );
}

function Routes() {
  const t = useT();
  const path = usePath().replace(/\/+$/, "") || "/";
  const camera = /^\/cameras\/([^/]+)(?:\/([^/]+))?$/.exec(path);
  if (camera) {
    return (
      <Suspense fallback={<Empty title={t("Loading camera…")} />}>
        <CameraPage id={decodeURIComponent(camera[1])} tab={camera[2]} />
      </Suspense>
    );
  }
  const prof = /^\/profiles\/([^/]+)\/([^/]+)\/([^/]+)$/.exec(path);
  if (prof) {
    const [vendor, model, version] = prof.slice(1).map(decodeURIComponent);
    return <ProfilePage key={path} id={`${vendor}/${model}`} version={version} />;
  }
  const Page = pages[path];
  if (!Page) return <Notice tone="warn">{t("Page not found: {path}", { path })}</Notice>;
  return <Page />;
}
