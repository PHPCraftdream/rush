import { SitterCarburetor } from "./SitterCarburetor";

// oxlint-disable-next-line carburetor/no-module-level-store -- client-only SPA: no server rendering, one store per page
export const sitterCarburetor = new SitterCarburetor({ sitter: { running: false } });
