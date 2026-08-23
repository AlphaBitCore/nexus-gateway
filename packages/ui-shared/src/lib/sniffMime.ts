// Magic-byte sniffing.
//
// This is the browser's half of a pair. The Go sniffer in
// transport/normalize/locator is the other; they cannot share code across
// the language boundary, so they share a vector table
// (transport/normalize/locator/testdata/sniff-vectors.json) and both assert
// against it. Before that table they disagreed, and the disagreement was
// visible to a user: the server served an iPhone photo inline as video/mp4
// and named the download .mp4 while this side called the same bytes
// image/heic.
//
// The wire-declared mime is caller-controlled, so it must never decide how
// bytes are rendered — a caller could label a script as an image. The type
// used to construct a Blob comes from the bytes themselves; the declared
// mime is only ever a display hint.

const starts = (b: Uint8Array, sig: number[], at = 0): boolean =>
  b.length >= at + sig.length && sig.every((v, i) => b[at + i] === v);

const ascii = (b: Uint8Array, s: string, at = 0): boolean =>
  starts(
    b,
    Array.from(s, (c) => c.charCodeAt(0)),
    at,
  );

/**
 * Returns the sniffed media type, or `application/octet-stream` when the
 * bytes match nothing known — the safe default, because unknown content
 * is downloaded rather than rendered.
 */
export function sniffMime(b: Uint8Array): string {
  if (starts(b, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) return 'image/png';
  if (starts(b, [0xff, 0xd8, 0xff])) return 'image/jpeg';
  if (ascii(b, 'GIF87a') || ascii(b, 'GIF89a')) return 'image/gif';
  if (ascii(b, 'RIFF') && ascii(b, 'WEBP', 8)) return 'image/webp';
  if (ascii(b, 'RIFF') && ascii(b, 'WAVE', 8)) return 'audio/wav';
  if (ascii(b, 'ftyp', 4)) return isobmff(b);
  if (starts(b, [0x49, 0x44, 0x33]) || starts(b, [0xff, 0xfb]) || starts(b, [0xff, 0xf3])) {
    return 'audio/mpeg';
  }
  if (ascii(b, 'fLaC')) return 'audio/flac';
  if (ascii(b, 'OggS')) return 'audio/ogg';
  if (ascii(b, '%PDF-')) return 'application/pdf';
  if (starts(b, [0x1a, 0x45, 0xdf, 0xa3])) return 'video/webm';
  return 'application/octet-stream';
}

/**
 * ISO base media brands. The same container carries still images, audio and
 * video, so the brand is the only thing that says which — and reading it
 * wrong is what served an iPhone photo as a movie.
 *
 * The brand list is kept identical to the Go sniffer's by the shared vector
 * table both sides assert against.
 */
function isobmff(b: Uint8Array): string {
  const brand = new TextDecoder('latin1').decode(b.slice(8, 12));
  switch (brand) {
    case 'heic':
    case 'heix':
    case 'hevc':
    case 'hevx':
      return 'image/heic';
    case 'mif1':
    case 'msf1':
      return 'image/heif';
    case 'avif':
    case 'avis':
      return 'image/avif';
    case 'M4A ':
    case 'M4B ':
    case 'M4P ':
      return 'audio/mp4';
  }
  return 'video/mp4';
}

/** File extension for a sniffed type, so a download opens in the OS. */
export function extensionForMime(mime: string): string {
  switch (mime) {
    case 'image/png':
      return '.png';
    case 'image/jpeg':
      return '.jpg';
    case 'image/gif':
      return '.gif';
    case 'image/webp':
      return '.webp';
    case 'image/heic':
      return '.heic';
    case 'image/heif':
      return '.heif';
    case 'image/avif':
      return '.avif';
    case 'audio/mp4':
      return '.m4a';
    case 'audio/wav':
      return '.wav';
    case 'audio/mpeg':
      return '.mp3';
    case 'audio/flac':
      return '.flac';
    case 'audio/ogg':
      return '.ogg';
    case 'video/mp4':
      return '.mp4';
    case 'video/webm':
      return '.webm';
    case 'application/pdf':
      return '.pdf';
    default:
      return '.bin';
  }
}
