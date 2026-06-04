// Package tui is the Bubble Tea operator console. Views are thin presenters over a
// Gateway (the typed core client); none of them build HTTP requests. The shared
// contract + leaf widgets live in the kit sub-package; this file re-exports the
// public names via type aliases so the cli's tui.Session / tui.Gateway references
// (and the in-package view code) keep working unchanged as the package is
// decomposed into kit / views / conversation / resource / wizard.
package shell

import "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"

// Session is the resolved render context (env + remembered model/VK selection).
type Session = kit.Session

// Gateway is the capability surface the views render over (*core.Client satisfies it).
type Gateway = kit.Gateway

// viewModel is one dashboard view (the in-package alias of kit.ViewModel).
type viewModel = kit.ViewModel

type textCapturer interface{ Capturing() bool }

// helpProvider lets a view supply its own bottom keybar text.
type helpProvider interface{ Help() string }

// leaver is implemented by views that hold a background stream/connection so the
// shell can tear it down when the operator navigates away.
type leaver interface{ Leave() }

// crumbProvider lets a view supply a dynamic breadcrumb segment (e.g. the Event
// view contributes the open event id) instead of its static registry name.
type crumbProvider interface{ Crumb() string }

// backHandler lets a view consume `esc` to close its own inner level (a detail
// drawer) before the root pops the nav stack. It returns true when it handled the
// back (the operator stays in the view), false to let the root walk up a level.
type backHandler interface{ Back() bool }

// highlighter is implemented by views that can spotlight a row the agent points
// at via the highlight canvas drive. Best-effort by design (§6): a view that
// does not implement it simply ignores the drive — the agent never hangs on it.
type highlighter interface{ Highlight(ref string) }
