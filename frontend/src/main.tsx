import "./index.css";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ApiError } from "@/api/client";
import { App } from "./App";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A 4xx will not change by asking again.
      retry: (count, err) => !(err instanceof ApiError && err.status < 500) && count < 2,
      refetchOnWindowFocus: false,
    },
  },
});

// A session that expires mid-use sends the user back to the login.
queryClient.getQueryCache().subscribe((e) => {
  if (e.type !== "updated" || e.action.type !== "error") return;
  const err = e.action.error;
  if (err instanceof ApiError && err.status === 401 && e.query.queryKey[0] !== "me") {
    queryClient.invalidateQueries({ queryKey: ["me"] });
  }
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
