import { createContext, useContext } from 'react';

/**
 * Where a media element's bytes can be fetched from.
 *
 * A context rather than four more props: only the leaf card needs these two
 * values, and threading them through the message, block and bubble
 * components would put a parameter on three signatures that have no use for
 * it. A surface that renders normalized payloads without providing this
 * still works — the card falls back to metadata-only, which is the honest
 * state for "these bytes are not reachable from here".
 */
export interface MediaByteOrigin {
  eventId: string;
  direction: 'request' | 'response';
}

export const MediaByteOriginContext = createContext<MediaByteOrigin | null>(null);

export function useMediaByteOrigin(): MediaByteOrigin | null {
  return useContext(MediaByteOriginContext);
}
