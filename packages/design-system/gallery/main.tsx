import { createRoot } from "react-dom/client";
import { StrictMode } from "react";
import { ToastProvider } from "../src/index.ts";
import { Gallery } from "./Gallery.tsx";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ToastProvider>
      <Gallery />
    </ToastProvider>
  </StrictMode>,
);
