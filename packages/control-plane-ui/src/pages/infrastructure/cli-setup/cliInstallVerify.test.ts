import { describe, it, expect } from 'vitest';
import enPages from '@/i18n/locales/en/pages.json';
import esPages from '@/i18n/locales/es/pages.json';
import zhPages from '@/i18n/locales/zh/pages.json';

// SEC-W4-02 regression: the in-product CLI install instructions must verify the
// downloaded binary against its published .sha256 sidecar BEFORE chmod/run, in
// every locale. A naive `curl | chmod +x` (the pre-fix state) lets a write to
// the served downloads dir ship attacker code to every operator. This test pins
// the verification step so it cannot silently regress out of the install copy.

type CliSetup = {
  installMacOSStep1: string;
  installLinuxStep1: string;
  installWindowsStep1: string;
};
const locales: Record<string, CliSetup> = {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  en: (enPages as any).infrastructure.cliSetup,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  es: (esPages as any).infrastructure.cliSetup,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  zh: (zhPages as any).infrastructure.cliSetup,
};

describe('SEC-W4-02 — CLI install instructions verify the download checksum', () => {
  for (const [loc, cli] of Object.entries(locales)) {
    describe(`locale ${loc}`, () => {
      it('macOS Step 1 downloads AND verifies via the darwin .sha256 sidecar before running', () => {
        const s = cli.installMacOSStep1;
        expect(s).toContain('nexus-cli-darwin-arm64-latest');
        // verification must reference the sidecar and run a sha256 check
        expect(s).toContain('nexus-cli-darwin-arm64-latest.sha256');
        expect(s).toMatch(/shasum -a 256 -c|sha256sum -c/);
      });

      it('Linux Step 1 verifies the .sha256 sidecar and only chmods after the check', () => {
        const s = cli.installLinuxStep1;
        expect(s).toContain('nexus-cli-linux-amd64-latest.sha256');
        expect(s).toContain('sha256sum -c');
        // fail-closed ordering: the checksum line must appear before chmod +x,
        // so a mismatch aborts (set -e in a copy-pasted block) before the binary
        // is made executable / run.
        expect(s.indexOf('sha256sum -c')).toBeLessThan(s.indexOf('chmod +x'));
      });

      it('Windows Step 1 compares Get-FileHash against the published sidecar and throws on mismatch', () => {
        const s = cli.installWindowsStep1;
        expect(s).toContain('nexus-cli-windows-amd64-latest.exe.sha256');
        expect(s).toContain('Get-FileHash');
        expect(s).toMatch(/-Algorithm SHA256/);
        expect(s).toContain('throw');
      });
    });
  }
});
