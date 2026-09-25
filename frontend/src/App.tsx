import { useQueryClient } from "@tanstack/react-query";
import { type ComponentType, useEffect } from "react";
import { errorMessage } from "@/api/client";
import { startLive, stopLive } from "@/api/live";
import { useMe } from "@/api/queries";
import { Layout, Logo } from "@/components/Layout";
import { Toaster } from "@/components/toast";
import { Notice } from "@/components/ui/card";
import { usePath } from "@/lib/router";
import { AssetsPage } from "@/pages/Assets";
import { CamerasPage } from "@/pages/Cameras";
import { EventsPage } from "@/pages/Events";
import { LoginPage } from "@/pages/Login";
import { ProfilesPage } from "@/pages/Profiles";
import { SettingsPage } from "@/pages/Settings";
import { TargetsPage } from "@/pages/Targets";

const pages: Record<string, ComponentType> = {
  "/": CamerasPage,
  "/cameras": CamerasPage,
  "/events": EventsPage,
  "/profiles": ProfilesPage,
  "/assets": AssetsPage,
  "/targets": TargetsPage,
  "/settings": SettingsPage,
};

export function App() {
  const qc = useQueryClient();
  const me = useMe();
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
        <Notice tone="error">Cannot reach the node: {errorMessage(me.error)}</Notice>
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
  const path = usePath();
  const Page = pages[path.replace(/\/+$/, "") || "/"];
  if (!Page) return <Notice tone="warn">Page not found: {path}</Notice>;
  return <Page />;
}
