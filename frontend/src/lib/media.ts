import type { Translate } from "./i18n";

/** A codec as cameras and clients name it. */
export function codecLabel(codec: string) {
  switch (codec) {
    case "h264":
      return "H.264";
    case "h265":
      return "H.265";
    case "mjpeg":
      return "MJPEG";
  }
  return codec.toUpperCase();
}

/** A stream by its role. */
export function streamLabel(name: string, t: Translate) {
  switch (name) {
    case "main":
      return t("Main stream");
    case "sub":
      return t("Sub stream");
    case "third":
      return t("Third stream");
  }
  return name;
}

/** What a stream is for, for someone new to cameras. */
export function streamPurpose(name: string, t: Translate) {
  switch (name) {
    case "main":
      return t("Best quality, for recording and full-screen viewing.");
    case "sub":
      return t("A light copy for grids of many cameras, phones and slow links.");
    case "third":
      return t("An extra stream, often MJPEG, for simple clients and web pages.");
  }
  return "";
}

/** One line summary: codec, resolution and frame rate. */
export function streamSummary(s: { codec: string; resolution: string; fps: number }) {
  return `${codecLabel(s.codec)} ${s.resolution} @ ${s.fps} fps`;
}
