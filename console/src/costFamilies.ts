// The cost families lux prices itself, shared by the console and its mock
// luxd. No imports: the mock server loads it outside the app bundle.

export const COMPUTE = "compute";
/** lux's own disk family: a figure apart from compute and from the plugins' families, counted under Show Compute. */
export const BLOCK_STORAGE = "block-storage";
/** A host's public IPv4 addresses: shared between its Runs like compute. */
export const PUBLIC_IP = "public-ip";
/** Network transfer, priced per placement (its own bytes) and per host (its own traffic, unallocated). */
export const DATA_TRANSFER = "data-transfer";
/** The host-tied families: what Show Compute counts (luxd's hostFamilies, rank=compute). */
export const HOST_FAMILIES: readonly string[] = [COMPUTE, BLOCK_STORAGE, PUBLIC_IP, DATA_TRANSFER];
