import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

export type Schemas = components["schemas"];
export type Camera = Schemas["Camera"];
export type CameraStream = Schemas["Stream"];
export type CameraState = Camera["status"]["state"];
export type Profile = Schemas["Profile"];
export type ProfileDetail = Schemas["ProfileDetail"];
export type ImportResult = Schemas["ImportResult"];
export type ImportReport = Schemas["ImportReport"];
export type Asset = Schemas["Asset"];
export type Target = Schemas["Target"];
export type TargetInput = Schemas["TargetInput"];
export type EventItem = Schemas["Event"];
export type EventPage = Schemas["EventPage"];
export type NodeInfo = Schemas["Node"];
export type NodeMetrics = Schemas["NodeMetrics"];
export type Settings = Schemas["Settings"];
export type Problem = Schemas["Problem"];
export type CreateCamera = Schemas["CreateCamera"];
export type Param = Schemas["Param"];
export type Protocol = Schemas["Protocol"];
export type NetworkInput = Schemas["NetworkInput"];
export type UpdateCamera = Schemas["UpdateCamera"];
export type CameraUserInput = Schemas["CameraUserInput"];
export type CloneCamera = Schemas["CloneCamera"];
export type Token = Schemas["Token"];
export type TokenInput = Schemas["TokenInput"];
export type CreatedToken = Schemas["CreatedToken"];
export type AuditEntry = Schemas["AuditEntry"];
export type AuditPage = Schemas["AuditPage"];
export type BulkAction = Schemas["BulkAction"];
export type BulkResult = Schemas["BulkResult"];
export type NodeSample = Schemas["NodeSample"];
export type Job = Schemas["Job"];
export type JobStatus = Job["status"];
export type JobDetail = Schemas["JobDetail"];
export type JobEvent = Schemas["JobEvent"];
export type JobPage = Schemas["JobPage"];

// Every state-changing request carries this header; cross-site forms cannot
// send it, which is part of the CSRF protection.
const csrfHeader = { "X-MockVision-Request": "1" };

export const api = createClient<paths>({
  baseUrl: "/api/v1",
  credentials: "same-origin",
  headers: csrfHeader,
});

/** An API failure carrying the problem+json document. */
export class ApiError extends Error {
  readonly problem: Problem;

  constructor(problem: Problem) {
    super(problem.detail || problem.title);
    this.problem = problem;
  }

  get status() {
    return this.problem.status;
  }

  /** Field errors keyed by field name. */
  fieldErrors(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const e of this.problem.errors ?? []) out[e.field] = e.message;
    return out;
  }
}

function toProblem(error: unknown, response: Response): Problem {
  if (error && typeof error === "object" && "title" in error) return error as Problem;
  return { type: "about:blank", title: response.statusText || "Request failed", status: response.status };
}

/** Returns the data of an openapi-fetch result or throws ApiError. */
export function unwrap<T>(res: { data?: T; error?: unknown; response: Response }): T {
  if (res.error !== undefined || !res.response.ok) {
    throw new ApiError(toProblem(res.error, res.response));
  }
  return res.data as T;
}

/** Uploads a file as multipart/form-data. */
export async function upload<T>(path: string, file: File): Promise<T> {
  const body = new FormData();
  body.append("file", file, file.name);
  const response = await fetch(`/api/v1${path}`, {
    method: "POST",
    body,
    credentials: "same-origin",
    headers: csrfHeader,
  });
  const data = await response.json().catch(() => undefined);
  if (!response.ok) throw new ApiError(toProblem(data, response));
  return data as T;
}

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const fields = err.problem.errors?.map((e) => (e.field ? `${e.field}: ${e.message}` : e.message));
    return fields?.length ? fields.join("; ") : err.message;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}
