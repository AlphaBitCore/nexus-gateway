import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { sniffMime, extensionForMime } from '../../src/lib/sniffMime';

// The browser's half of a pair. The Go sniffer in
// transport/normalize/locator is the other, and the two cannot share code —
// so they share this table and both assert against it.
//
// Before it they disagreed, and a user could see it: the server served an
// iPhone photo inline as video/mp4 and named the download .mp4, while this
// side called the same bytes image/heic.
// Resolved from the package root, which is where vitest runs. An absolute
// path would not survive a checkout elsewhere, and import.meta.url is not a
// file URL under this runner.
const VECTORS_PATH = resolve(
  process.cwd(),
  '../shared/transport/normalize/locator/testdata/sniff-vectors.json',
);

interface Vector {
  name: string;
  prefix: string;
  mime: string;
  ext: string;
}

function loadVectors(): Vector[] {
  const doc = JSON.parse(readFileSync(VECTORS_PATH, 'utf8')) as { vectors: Vector[] };
  return doc.vectors;
}

const bytes = (hex: string): Uint8Array =>
  Uint8Array.from(hex.match(/.{1,2}/g) ?? [], (h) => parseInt(h, 16));

describe('sniffMime against the shared vector table', () => {
  const vectors = loadVectors();

  it('reads the same table the Go sniffer does', () => {
    // If this file moves or the table shrinks, the two sniffers are free to
    // drift again — which is the failure, not the missing file.
    expect(vectors.length).toBeGreaterThanOrEqual(20);
  });

  it.each(vectors.map((v) => [v.name, v] as const))('%s', (_name, v) => {
    expect(sniffMime(bytes(v.prefix))).toBe(v.mime);
    expect(extensionForMime(v.mime)).toBe(`.${v.ext}`);
  });
});

describe('ISO base media brands', () => {
  const brand = (s: string) =>
    Uint8Array.from([0, 0, 0, 0x1c, ...Array.from('ftyp' + s, (c) => c.charCodeAt(0))]);

  it.each([
    ['heic', 'image/heic'],
    ['heix', 'image/heic'],
    ['mif1', 'image/heif'],
    ['msf1', 'image/heif'],
    ['avif', 'image/avif'],
    ['avis', 'image/avif'],
    ['M4A ', 'audio/mp4'],
    ['isom', 'video/mp4'],
    ['mp42', 'video/mp4'],
  ])('%s -> %s', (b, want) => {
    expect(sniffMime(brand(b))).toBe(want);
  });
});
