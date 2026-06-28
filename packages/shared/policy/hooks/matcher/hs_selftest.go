//go:build vectorscan

package matcher

/*
#include <hs/hs.h>
#include <stdlib.h>

static int nexus_selftest_cb(unsigned int id, unsigned long long from,
                             unsigned long long to, unsigned int flags, void *ctx) {
    (void)id;(void)from;(void)to;(void)flags;
    *(int*)ctx = *(int*)ctx + 1;
    return 0;
}

// nexus_hs_selftest compiles /secret/ and scans "a secret here". It writes the
// library version, the scan return code, and the match count through out params.
static void nexus_hs_selftest(const char **ver, int *scanRC, int *matches) {
    *ver = hs_version();
    *scanRC = 0; *matches = 0;
    hs_database_t *db = NULL;
    hs_compile_error_t *ce = NULL;
    if (hs_compile("secret", 0, HS_MODE_BLOCK, NULL, &db, &ce) != HS_SUCCESS) {
        *scanRC = -1000; // compile failed
        if (ce) hs_free_compile_error(ce);
        return;
    }
    hs_scratch_t *sc = NULL;
    hs_error_t arc = hs_alloc_scratch(db, &sc);
    if (arc != HS_SUCCESS) {
        *scanRC = -2000 + (int)arc; hs_free_database(db); return;
    }
    int count = 0;
    *scanRC = (int)hs_scan(db, "a secret here", 13, 0, sc, nexus_selftest_cb, &count);
    *matches = count;
    hs_free_scratch(sc);
    hs_free_database(db);
}
*/
import "C"

// HSSelfTest reports whether the linked Hyperscan/Vectorscan library actually
// matches at runtime: it compiles /secret/ and scans "a secret here", returning
// the library version, the hs_scan return code, and the match count. A healthy
// library returns (ver, 0, 1). Any other result means content scanning is
// silently non-functional on this host even though compilation "succeeds".
func HSSelfTest() (version string, scanRC int, matches int) {
	var ver *C.char
	var rc, m C.int
	C.nexus_hs_selftest(&ver, &rc, &m)
	return C.GoString(ver), int(rc), int(m)
}
