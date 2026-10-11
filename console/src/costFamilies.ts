// The cost families lux prices itself, shared by the console and its mock
// luxd. No imports: the mock server loads it outside the app bundle.

export const COMPUTE = "compute";
/** lux's own disk family: a figure apart from compute and from the plugins' families, counted under Show Compute. */
export const BLOCK_STORAGE = "block-storage";
/** The host-tied families: what Show Compute counts (luxd's hostFamilies, rank=compute). */
export const HOST_FAMILIES: readonly string[] = [COMPUTE, BLOCK_STORAGE];
