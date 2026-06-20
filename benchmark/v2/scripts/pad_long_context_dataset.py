#!/usr/bin/env python3
"""
S-02 Long-Context Dataset Padder
=================================

Pads benchmark/v2/datasets/long_context_v2.json so every prompt reaches a true
~16,000-token context window (measured as len(text)//4, the standard cl100k_base
approximation for gpt-4o-mini). Without this, S-02 prompts are one-line topic
instructions (~65 tokens) and the scenario is indistinguishable from S-01.

Design notes
------------
* IDEMPOTENT: each prompt's original instruction is the text *before* the
  CONTEXT marker. Re-running re-pads from the extracted instruction rather than
  double-padding an already-padded prompt.
* SELF-CONTAINED: all domain seed content is inline below — no external files,
  no new dependencies (stdlib only).
* PADDING NATURE: hitting ~60k chars of *uniquely authored* prose per topic
  without an LLM at runtime is not feasible, so each context body is a long,
  structured reference document composed from a curated pool of REAL,
  topic-specific domain paragraphs (TSMC share data, CHIPS Act, Fed rate path,
  Raft/Paxos, Black-Scholes, etc.) recombined under distinct analytical section
  headers (Overview / History / Technical / Data / Risk / Outlook). This is
  coherent, domain-appropriate prose — NOT lorem ipsum and NOT random garbage —
  which is exactly what a context-window/latency benchmark needs: enough tokens
  of on-topic text that the model produces a real, non-degenerate response.
  The facts recur across sections (as they would in a long real report); the
  framing varies so the document reads coherently end to end.

Output
------
* datasets/long_context_v2_padded.json  (the padded set, version "v2-padded")
* datasets/long_context_v2.json          (overwritten so s02_long_context.py,
                                           which calls load_prompts(
                                           "long_context_v2.json"), picks it up
                                           WITHOUT modification — see the
                                           non-goal in the task spec)

Run from benchmark/v2/:  python3 scripts/pad_long_context_dataset.py
"""
from __future__ import annotations

import json
from pathlib import Path

DATASETS = Path(__file__).resolve().parent.parent / "datasets"
SRC = DATASETS / "long_context_v2.json"
PADDED = DATASETS / "long_context_v2_padded.json"

TARGET_TOKENS = 16_000
# CALIBRATION (fixed 2026-06-16): the cl100k tokenizer averages ~5.1 chars/token
# on this structured prose, NOT the 4.0 the old len//4 proxy assumed. The first
# cut (63.6k chars) measured ~16k by len//4 but only ~12,570 REAL tokens on the
# AWS runner — below the 14k floor for a long-context test. Target the char count
# off the REAL chars/token ratio so prompts land at ~16k *real* tokens.
CHARS_PER_TOKEN = 5.1            # fallback ratio if tiktoken is unavailable
# Body target in REAL tokens. The full prompt = instruction (~65 tok) + markers +
# body + repeated tail (~70 tok) ≈ body + 140. Target body so the whole prompt
# lands ~16k tokens, inside the ideal 15,500–16,500 band.
BODY_TARGET_TOKENS = TARGET_TOKENS - 140
CONTEXT_MARKER = "\n\n--- CONTEXT ---\n\n"
END_MARKER = "\n\n--- END CONTEXT ---\n\n"

# ---------------------------------------------------------------------------
# Curated real-domain seed paragraphs, keyed by the UUID prefix of each prompt.
# Each list element is a self-contained paragraph of genuine domain content
# covering the bullets the task spec required for that topic.
# ---------------------------------------------------------------------------
SEEDS: dict[str, list[str]] = {
    # 1 — Semiconductor industry
    "af3e0bf1": [
        "As of 2026, TSMC retains roughly 60% of global foundry revenue and well above 90% of leading-edge (3nm and below) capacity, with Samsung Foundry a distant second near 11% and Intel Foundry Services attempting to re-enter the merchant market via its 18A node. The structural moat is not merely fabrication: it is the co-optimized ecosystem of EDA tooling, IP libraries, and packaging that compounds yield advantages over successive nodes.",
        "The U.S. CHIPS and Science Act allocated roughly $52.7 billion in subsidies and a 25% investment tax credit, anchoring TSMC Arizona (Fab 21), Intel Ohio, Samsung Taylor (Texas), and Micron's memory build-outs. The policy intent is to lift U.S. domestic leading-edge share from near 0% back toward 20% by the end of the decade, but the binding constraints are skilled-labor availability and the 30–50% cost premium of U.S. fabrication versus Taiwan.",
        "High-bandwidth memory (HBM) became the defining bottleneck of the AI build-out. HBM3E stacks command 5–8x the per-bit margin of commodity DDR5, and SK Hynix, Samsung, and Micron have effectively sold out 2025–2026 capacity to NVIDIA and the hyperscalers. HBM demand is forecast to grow 40–50% CAGR through 2027, pulling TSV (through-silicon via) and CoWoS advanced-packaging capacity along with it.",
        "EUV lithography economics center on ASML's monopoly: a single EUV scanner exceeds $200M, a High-NA EUV system approaches $380M, and a leading-edge fab requires dozens. The capital intensity — $20–30B per leading-edge fab — is the primary barrier to entry and the reason only three firms remain at the frontier. Each node transition roughly doubles design cost, pushing tape-out costs for a 3nm SoC past $500M.",
        "Lead times normalized unevenly after the 2020–2022 shortage. Leading-edge logic returned to 12–16 week lead times by 2024, but mature-node analog, power management, and microcontroller parts — concentrated at the >28nm nodes critical to automotive — remained volatile into 2025 as automakers rebuilt buffer inventory after the just-in-time collapse.",
        "Export controls escalated in a stepwise timeline: October 2022 controls on advanced compute and equipment to China, October 2023 tightening on A100/H100-class accelerators and the addition of performance-density thresholds, and 2024–2025 expansions covering HBM and additional toolmakers. The controls reshaped NVIDIA's China-specific SKUs (A800/H800 then H20) and accelerated indigenous Chinese efforts at SMIC, which reached 7nm-class production via DUV multi-patterning at low yield and high cost.",
        "The competitive dynamic is increasingly a packaging race. As transistor scaling slows, chiplet architectures (AMD's Infinity Fabric, Intel's Foveros and EMIB, TSMC's CoWoS and SoIC) let firms compose heterogeneous dies. Advanced packaging capacity, not wafer starts, is now the gating factor for AI accelerators, and TSMC's CoWoS expansion roadmap is watched as closely as its node cadence.",
        "Demand segmentation matters for the thesis: data-center AI silicon is supply-constrained and margin-rich; smartphone application processors are mature and cyclical; automotive and industrial are growing structurally with electrification; and PC/consumer is the most cyclical. A diversified foundry like TSMC smooths these cycles, whereas memory makers ride violent boom-bust swings tied to bit-supply elasticity.",
    ],
    # 2 — Monetary policy
    "a411a2e5": [
        "The Federal Reserve's policy path from 2015 to 2026 traces a full cycle: liftoff from the zero lower bound in December 2015, a gradual climb to 2.25–2.50% by 2018, emergency cuts to zero in March 2020, the most aggressive tightening in four decades from March 2022 (raising the funds rate from 0–0.25% to 5.25–5.50% by mid-2023), and a measured easing cycle beginning in 2024 as disinflation took hold.",
        "Quantitative easing and tightening operate through balance-sheet mechanics. The Fed's balance sheet expanded from ~$4.2T pre-pandemic to ~$8.9T at its 2022 peak via Treasury and MBS purchases, then ran off via QT at a capped pace (initially up to $95B/month). QE compresses term premia and signals lower-for-longer rates; QT reverses this, draining reserves and steepening the curve, with money-market plumbing (the reverse repo facility and reserve scarcity) as the key transmission risk.",
        "The transmission mechanism runs through several channels: the interest-rate channel (policy rate to market rates to investment and consumption), the credit channel (bank lending standards and balance-sheet capacity), the asset-price/wealth channel (equity and housing valuations), and the exchange-rate channel (rate differentials to the dollar to net exports). Each channel operates with long and variable lags — Friedman's classic 12–18 month caveat.",
        "The 2022–2023 inflation episode peaked at 9.1% CPI in June 2022, driven by a confluence of pandemic goods-demand shifts, fiscal stimulus, supply-chain dislocation, and the Russia-Ukraine energy and food shock. The debate between 'transitory' and 'persistent' framings hinged on whether the impulse would propagate into wages and services inflation; sticky services and shelter inflation ultimately validated the more hawkish view and forced the rapid tightening.",
        "The ECB and BOJ diverged sharply. The ECB lifted its deposit rate from -0.50% to 4.00% by 2023, constrained by fragmentation risk across periphery sovereign spreads (addressed via the Transmission Protection Instrument). The BOJ maintained yield-curve control and negative rates far longer, only exiting negative rates in 2024 — a historic normalization after decades of deflationary policy.",
        "The Taylor Rule provides a benchmark: r = r* + π + 0.5(π − π*) + 0.5(output gap). Plugging in a neutral real rate near 0.5%, a 2% target, and the 2022 inflation overshoot implied a prescribed rate well above 6% at the peak — above where the Fed actually went, illustrating why some critics argued policy was behind the curve in 2021–2022.",
        "Credit spreads are a real-time transmission gauge. Investment-grade and high-yield spreads widened materially during the 2022 tightening and the March 2023 regional-bank stress (SVB, Signature, First Republic), then compressed as the Fed's emergency facilities (the Bank Term Funding Program) stabilized deposit flight. Spread behavior demonstrates how financial conditions, not just the policy rate, mediate the real-economy impact.",
        "Forward guidance and the 'dot plot' (the Summary of Economic Projections) became central tools: by shaping expectations of the future rate path, the Fed influences the entire yield curve today. The credibility of guidance depends on the central bank's inflation-fighting reputation — anchored long-run expectations let policymakers look through transitory shocks without losing control of the nominal anchor.",
    ],
    # 3 — Distributed systems
    "d27c51a6": [
        "Raft is a consensus algorithm designed for understandability. It decomposes consensus into leader election, log replication, and safety. A node is follower, candidate, or leader; terms act as a logical clock. On election timeout a follower becomes candidate, increments its term, and requests votes; a candidate winning a majority becomes leader and sends periodic AppendEntries heartbeats. The leader appends client commands to its log and replicates them; an entry is committed once stored on a majority, after which it is applied to the state machine.",
        "Raft's safety rests on the Log Matching Property and the Leader Completeness Property: if two logs contain an entry with the same index and term, they are identical up to that index; and a leader for a given term contains all entries committed in prior terms. The election restriction — a candidate must have an at-least-as-up-to-date log to win votes — guarantees committed entries are never lost across leader changes.",
        "Paxos predates Raft and is the theoretical foundation. Single-decree Paxos has proposers, acceptors, and learners, proceeding in prepare/promise and accept/accepted phases keyed by monotonically increasing proposal numbers. Multi-Paxos amortizes the prepare phase by electing a stable leader. Variants include Fast Paxos (fewer message delays at the cost of larger quorums), EPaxos (leaderless, exploiting commutativity), and Flexible Paxos (decoupling the quorum-intersection requirement).",
        "Google Spanner combines Paxos-replicated tablets with TrueTime — a globally synchronized clock with bounded uncertainty (epsilon) backed by GPS and atomic clocks. By waiting out the uncertainty interval on commit, Spanner provides external consistency (linearizability) for globally distributed transactions, a property most systems cannot offer without such a clock substrate.",
        "CockroachDB layers a SQL engine over a transactional, range-partitioned key-value store; each range is a Raft group, and distributed transactions use a parallel-commit protocol with hybrid-logical clocks rather than TrueTime hardware. Apache Cassandra takes the opposite stance: leaderless, Dynamo-style with consistent hashing, tunable consistency (ONE/QUORUM/ALL), and last-write-wins or CRDT-based conflict resolution, trading linearizability for availability and write throughput.",
        "Vector clocks capture causality: each node maintains a per-node counter vector; events are concurrent if neither vector dominates the other. They detect conflicts in eventually consistent stores but grow with the number of writers, motivating dotted version vectors and server-side pruning in production systems.",
        "Conflict-free replicated data types (CRDTs) provide convergence without coordination. State-based (CvRDT) types merge via a join on a semilattice (monotone, commutative, idempotent), while operation-based (CmRDT) types require causal delivery of commutative operations. Counters, OR-Sets, and sequence CRDTs (RGA, LSEQ) power collaborative editing and offline-first applications where availability trumps strong consistency.",
        "Replication-factor tradeoffs are governed by quorum intersection: with N replicas, choosing read quorum R and write quorum W such that R + W > N guarantees a read sees the latest write. Higher W improves durability but hurts write latency and availability under partition; the CAP theorem formalizes that under a network partition a system must sacrifice either consistency or availability, and PACELC extends this to the latency-vs-consistency tradeoff even absent partitions.",
    ],
    # 4 — Value vs growth investing
    "021a7132": [
        "Benjamin Graham's framework, inherited by Warren Buffett, distinguishes price from value and insists on a margin of safety — buying well below conservative intrinsic value to absorb error and misfortune. Buffett's evolution under Charlie Munger's influence shifted from Graham's cigar-butt bargains toward 'wonderful businesses at fair prices,' emphasizing durable competitive moats, high returns on incremental capital, and able, honest management.",
        "Munger's mental-models approach and his insistence on quality compounding reframed value investing: a business earning 20% on capital and reinvesting it will, over decades, dwarf a statistically cheap but stagnant one. His Daily Journal and Berkshire commentary stress avoiding stupidity over seeking brilliance, and the psychology of misjudgment as the investor's central enemy.",
        "Peter Lynch's 'invest in what you know,' articulated in One Up on Wall Street, popularized growth-at-a-reasonable-price (GARP) and the PEG ratio, classifying companies as slow growers, stalwarts, fast growers, cyclicals, turnarounds, and asset plays. Seth Klarman's Margin of Safety extends Graham into modern markets, emphasizing absolute (not relative) returns, the danger of forced selling, and treating cash and patience as positions.",
        "Valuation multiples each encode assumptions. P/E is simple but distorted by leverage, non-cash charges, and cyclicality. P/FCF captures actual cash generation and is harder to manipulate. EV/EBITDA neutralizes capital structure and is preferred for cross-company and M&A comparison, though it ignores capital intensity and can flatter asset-heavy businesses. No single multiple is sufficient; triangulation across several, plus a DCF cross-check, is standard practice.",
        "The Fama-French research program decomposed equity returns into systematic factors. The three-factor model added size (SMB) and value (HML, high book-to-market) to the market factor; the five-factor model added profitability (RMW) and investment (CMA). Empirically, the value premium was robust for decades but suffered a deep, prolonged drawdown from roughly 2017 to 2020 as growth and mega-cap technology dominated, reviving debate over whether the premium is compensation for risk or a behavioral anomaly being arbitraged away.",
        "Behavioral finance, via Kahneman and Tversky's prospect theory and Richard Thaler's work, explains persistent mispricing: loss aversion, anchoring, recency bias, herding, and overconfidence cause prices to deviate from fundamentals. Value investing is, in this lens, a structural harvest of other participants' behavioral errors — but it requires the temperament to endure extended underperformance and career risk.",
        "Growth investing prioritizes the trajectory and size of future cash flows over current cheapness, accepting high multiples when the addressable market, unit economics, and competitive position justify durable compounding. The discipline failure mode is paying any price for narrative; the value failure mode is the value trap — statistically cheap businesses in secular decline whose cheapness is rational.",
        "The synthesis most practitioners reach is that 'value' and 'growth' are not opposites but inputs to a single intrinsic-value calculation: growth is a component of value, valuable only when it earns returns above the cost of capital. The reconciliation reframes the debate from style boxes to the quality and price of future free cash flow.",
    ],
    # 5 — HTTPS / TLS / networking
    "23839121": [
        "An HTTPS request begins with DNS resolution, increasingly over encrypted transports: DNS-over-HTTPS (RFC 8484) and DNS-over-TLS (RFC 7858) prevent on-path observation of the queried hostname, though Server Name Indication and Encrypted Client Hello (ECH) address leakage at the TLS layer. The resolver returns A/AAAA records, after which the client opens a TCP (or QUIC/UDP) connection to the resolved address.",
        "TLS 1.3 (RFC 8446) streamlined the handshake to a single round trip. The ClientHello carries supported cipher suites, key-share extensions (an ephemeral ECDHE public key), and SNI. The server responds with ServerHello (its key share), then encrypted Certificate, CertificateVerify, and Finished messages. Both sides derive shared secrets via HKDF from the ECDHE exchange, and 0-RTT resumption lets returning clients send early data at the cost of replay exposure.",
        "Certificate-chain validation walks from the leaf certificate up to a trusted root in the OS/browser trust store. Each certificate's signature is verified against its issuer's public key; the validator checks validity dates, the Basic Constraints CA flag, key usage and extended key usage, name constraints, and that the leaf's Subject Alternative Name matches the requested host. A break anywhere — expired intermediate, untrusted root, name mismatch — fails the connection.",
        "Revocation is checked via Certificate Revocation Lists (CRLs) or the Online Certificate Status Protocol (OCSP). OCSP stapling (the Certificate Status Request extension) lets the server attach a time-stamped, CA-signed OCSP response to the handshake, avoiding a client-side round trip to the CA and the associated privacy leak, with OCSP Must-Staple pinning the requirement.",
        "Once TLS is established, HTTP/2 multiplexes many streams over one connection using binary framing — HEADERS, DATA, SETTINGS, WINDOW_UPDATE, and others — eliminating the head-of-line blocking of HTTP/1.1 pipelining at the application layer. HPACK compresses headers via a shared dynamic table plus Huffman coding, dramatically reducing redundant header bytes across requests.",
        "HTTP/3 runs over QUIC, which itself runs over UDP and embeds TLS 1.3. QUIC eliminates TCP-layer head-of-line blocking by giving each stream independent loss recovery, integrates the transport and cryptographic handshakes for faster connection establishment, and supports connection migration across network changes via connection IDs — valuable for mobile clients switching between Wi-Fi and cellular.",
        "Congestion control and flow control still govern throughput. QUIC reimplements TCP-like algorithms (CUBIC, BBR) in user space, enabling faster iteration than kernel TCP stacks. Flow control operates per-stream and per-connection via window updates, preventing a fast sender from overwhelming a slow receiver.",
        "The response then flows back: the server's application emits status, headers, and body framed into DATA frames; intermediaries (CDNs, reverse proxies) may terminate TLS, apply caching per Cache-Control and ETag semantics, and re-originate to the backend. The browser parses the response, and for HTML, begins the critical-rendering-path work of DOM/CSSOM construction, often triggering further HTTPS requests for subresources.",
    ],
    # 6 — Private equity
    "f01027b6": [
        "A leveraged buyout finances an acquisition with a mix of equity and substantial debt secured against the target's assets and cash flows. A representative structure funds a purchase at, say, 10x EBITDA with 50–60% debt (a blend of senior term loans, second lien, and high-yield or mezzanine), using the target's free cash flow to service and amortize debt. Returns are driven by deleveraging, EBITDA growth, and multiple expansion at exit.",
        "IRR and MOIC measure different things. MOIC (multiple of invested capital) is gross cash returned divided by cash invested — a 3.0x MOIC triples the money regardless of time. IRR is the time-weighted annualized return and is highly sensitive to holding period and the timing of cash flows; a dividend recapitalization that returns capital early can lift IRR sharply even at the same MOIC. GPs are evaluated on both, plus DPI (realized) and TVPI (total value).",
        "Fund economics follow the '2 and 20' template: roughly a 2% annual management fee on committed (then invested) capital, and 20% carried interest on profits above an 8% preferred return (hurdle), typically with a GP catch-up and either deal-by-deal or whole-fund (European) waterfall. The preferred return aligns the GP to clear a minimum bar before sharing in upside.",
        "Vintage-year matters enormously: funds raised at cyclical peaks (e.g., 2006–2007) deployed into high entry multiples and underperformed, while post-crisis vintages (2009–2011) bought cheaply and posted strong returns. Persistence of GP outperformance has weakened as the asset class matured and capital flooded in, compressing the dispersion that once rewarded top-quartile manager selection.",
        "KKR, Blackstone, and Apollo evolved from pure buyout shops into diversified alternative-asset managers spanning credit, real estate, infrastructure, and insurance. Blackstone surpassed $1 trillion in assets under management, with much growth in perpetual-capital and credit vehicles that smooth the fee base relative to episodic carry. Apollo's tie-up with Athene exemplifies the convergence of private equity and insurance balance sheets as a source of permanent capital.",
        "Operational value creation has displaced financial engineering as the dominant return narrative. Modern playbooks deploy operating partners to drive pricing optimization, procurement savings, salesforce effectiveness, working-capital reduction, and bolt-on M&A (the buy-and-build strategy), aiming to grow EBITDA rather than rely on leverage and multiple expansion alone in a higher-rate environment.",
        "Exit routes include strategic sale, secondary buyout (sale to another sponsor), and IPO. Higher interest rates after 2022 raised financing costs, compressed entry/exit multiples, and slowed exit activity, lengthening holding periods and elevating the appeal of continuation-vehicle secondaries that let GPs hold winning assets longer while offering LPs liquidity.",
        "Risk factors include over-leverage into a downturn (covenant breaches, refinancing walls), reliance on multiple expansion that may not recur, J-curve drag in early fund years as fees precede realizations, and limited liquidity. Due diligence emphasizes quality of earnings, customer concentration, cyclicality, and the durability of the cash flows that the debt structure assumes.",
    ],
    # 7 — Microservices
    "7699478f": [
        "The microservices style decomposes an application into independently deployable services organized around business capabilities, each owning its data and communicating over the network. Netflix's migration off a monolith to hundreds of services on AWS, and the open-sourcing of the Netflix OSS stack (Eureka for discovery, Ribbon for client-side load balancing, Hystrix for circuit breaking, Zuul for edge routing), defined early patterns the industry adopted.",
        "Kubernetes became the de facto orchestration substrate, scheduling containers across nodes, managing rollouts and self-healing, and exposing services via ClusterIP/NodePort/LoadBalancer and Ingress. A service mesh (Istio or Linkerd) adds an L7 data plane of sidecar proxies (Envoy in Istio's case) that handle mTLS, retries, timeouts, traffic shifting (canary, blue-green), and policy enforcement without application code changes.",
        "Distributed data consistency is the hardest problem. The Saga pattern coordinates a sequence of local transactions across services, each with a compensating action to undo on failure, via either choreography (event-driven) or orchestration (a central coordinator). It trades the atomicity of two-phase commit (2PC) for availability and loose coupling — 2PC's blocking coordinator and locking are poorly suited to autonomous, scalable services.",
        "Observability rests on three pillars: metrics, logs, and traces. OpenTelemetry standardizes instrumentation and context propagation; a trace context (trace ID and span IDs) propagates via W3C traceparent headers across service hops, letting a backend like Jaeger or Tempo reconstruct the full request path and latency breakdown across dozens of services.",
        "Asynchronous messaging decouples services. Apache Kafka provides a partitioned, replicated, append-only log; producers write to partitions, and consumer groups divide partitions among members for parallel consumption. Rebalancing reassigns partitions when members join or leave; the cooperative-sticky assignor and static membership reduce the 'stop-the-world' rebalance pauses that plagued earlier consumer-group protocols.",
        "API design favors well-defined contracts: REST over HTTP with OpenAPI specifications, gRPC with Protocol Buffers for low-latency internal RPC, and event schemas governed by a schema registry (Avro/Protobuf) to manage compatibility. Backward- and forward-compatible schema evolution is essential because services deploy independently and cannot assume synchronized upgrades.",
        "Resilience patterns guard against cascading failure: circuit breakers trip after a failure threshold to fail fast, bulkheads isolate resource pools, timeouts and exponential-backoff retries with jitter bound tail latency, and rate limiting and load shedding protect against overload. The anti-pattern is the 'distributed monolith' — services so chatty and coupled that they share the downsides of both architectures.",
        "Operational maturity demands CI/CD per service, infrastructure as code, centralized configuration and secrets management, and a platform/SRE function. The organizational corollary is Conway's Law: service boundaries tend to mirror team boundaries, so microservices succeed only when paired with autonomous, cross-functional teams that own their services end to end.",
    ],
    # 8 — Options / derivatives
    "ead6f87c": [
        "The Black-Scholes-Merton model prices a European option by constructing a continuously rebalanced, risk-free hedged portfolio of the option and the underlying. Under geometric Brownian motion with constant volatility and rate, the no-arbitrage argument yields the BSM partial differential equation, whose solution gives the closed-form call price C = S0 N(d1) − K e^(−rT) N(d2), with d1 = [ln(S0/K) + (r + σ²/2)T] / (σ√T) and d2 = d1 − σ√T.",
        "The Greeks are the model's risk sensitivities. Delta (∂C/∂S) is the hedge ratio and the N(d1) term; Gamma (∂²C/∂S²) measures how delta changes and is largest at-the-money near expiry; Vega is sensitivity to volatility; Theta is time decay; and Rho is rate sensitivity. Delta-hedging neutralizes first-order price risk, but the residual P&L is dominated by the gamma-theta tradeoff: a long-gamma position pays for itself through realized volatility exceeding the implied volatility embedded in the option's time decay.",
        "The binomial (Cox-Ross-Rubinstein) tree discretizes the underlying's evolution into up/down moves with risk-neutral probabilities, pricing by backward induction from expiry. It handles American early exercise naturally (compare intrinsic value to continuation value at each node) and converges to Black-Scholes as the number of steps grows — a pedagogically and practically vital bridge between discrete and continuous models.",
        "The VIX is the market's 30-day forward expectation of S&P 500 volatility, computed not from a single option but from a variance-swap-style replication: a weighted strip of out-of-the-money puts and calls across strikes, interpolated to a constant 30-day tenor. It is quoted in annualized volatility points and spikes during stress as demand for downside protection bids up put premia.",
        "Implied volatility is not constant across strikes or maturities, contradicting Black-Scholes's assumption. The volatility 'smile' or 'skew' — typically a put skew in equity indices, where downside strikes carry higher implied vol — reflects crash risk and demand for protection. The full surface (implied vol as a function of strike and maturity) is constructed and arbitrage-checked (no calendar or butterfly arbitrage) to price exotics and interpolate consistently.",
        "Delta-hedging P&L attribution decomposes a hedged option position into the gamma-rebalancing P&L (proportional to realized variance), the theta bleed (the cost of holding the option), and vega/vanna/volga effects from changes in the vol surface. The clean result for a continuously delta-hedged option is that P&L ≈ ½ Γ S² (σ_realized² − σ_implied²) integrated over the holding period — the realized-versus-implied vol spread is the trade.",
        "Zero-days-to-expiry (0DTE) options exploded in volume after the introduction of daily-expiring S&P 500 options, now a large share of index option volume. Their extreme gamma near expiry means dealer hedging flows can amplify intraday moves, and their convexity makes them lottery-like for buyers and dangerous for under-hedged sellers — a structural shift in intraday market microstructure.",
        "The term structure of volatility — how implied vol varies with maturity — is typically upward sloping in calm regimes (contango) and inverts during stress (backwardation) as near-term uncertainty spikes. Variance and volatility swaps let participants trade realized volatility directly, and the spread between implied and subsequently realized vol is the volatility risk premium that systematic option-selling strategies attempt to harvest.",
    ],
    # 9 — ESG
    "82378311": [
        "The EU's regulatory architecture leads global ESG disclosure. The Sustainable Finance Disclosure Regulation (SFDR) classifies funds as Article 6 (no sustainability focus), Article 8 ('light green,' promoting environmental/social characteristics), or Article 9 ('dark green,' with sustainable investment as the objective), forcing managers to substantiate marketing claims. The Corporate Sustainability Reporting Directive (CSRD) vastly expands the universe of firms required to report under the European Sustainability Reporting Standards (ESRS), introducing 'double materiality.'",
        "Double materiality is the conceptual core of CSRD: companies must report both how sustainability issues affect the enterprise (financial materiality) and how the enterprise affects people and the environment (impact materiality). This contrasts with the more investor-centric, single-financial-materiality lens of the ISSB/IFRS S1 and S2 standards, and reconciling the two frameworks is an ongoing global harmonization effort.",
        "MSCI's ESG rating methodology scores issuers AAA to CCC on financially material, industry-specific key issues, weighting exposure against management. Critics note rating divergence: the correlation between major providers' ESG scores is low (often cited near 0.4–0.5), far below the near-unity correlation of credit ratings, because providers disagree on what to measure, how to weight it, and how to handle disclosure gaps.",
        "Carbon accounting follows the GHG Protocol's three scopes: Scope 1 (direct emissions from owned sources), Scope 2 (indirect emissions from purchased electricity, steam, heat), and Scope 3 (all other value-chain emissions — purchased goods, use of sold products, business travel, investments). Scope 3 is the largest and least reliable, often exceeding 70% of a company's footprint, and its estimation methodology is a central point of contention.",
        "The Task Force on Climate-related Financial Disclosures (TCFD) framework, now folded into ISSB standards, structures disclosure around governance, strategy, risk management, and metrics/targets, and popularized climate scenario analysis — testing resilience against pathways such as orderly transition, disorderly transition, and 'hot house world' (e.g., NGFS scenarios) to surface transition and physical risks.",
        "Stranded-asset risk models quantify the value at risk if climate policy or technology shifts render assets (fossil reserves, carbon-intensive plants) uneconomic before the end of their useful lives. Carbon-budget analysis and the concept of 'unburnable carbon' underpin these models, which feed into both divestment debates and the pricing of transition risk in credit and equity.",
        "Greenwashing enforcement has intensified. Regulators (the SEC's climate-disclosure rulemaking and enforcement against misleading ESG fund labels, the EU's scrutiny under SFDR, and national authorities) have penalized overstated sustainability claims, prompting a wave of fund 'reclassifications' from Article 9 to Article 8 as managers retreated from claims they could not substantiate under tightening definitions.",
        "The investment debate splits between integration and impact. ESG integration treats material sustainability factors as inputs to risk-adjusted return, defensible on fiduciary grounds; values-based or impact investing accepts potential return tradeoffs for non-financial objectives. The politicization of ESG, especially in the United States, added anti-ESG legislation and fiduciary-duty disputes that reshaped how managers brand and market sustainability strategies.",
    ],
    # 10 — Recommender systems
    "299244f5": [
        "The Netflix Prize (2006–2009) catalyzed modern recommender research by offering $1M for a 10% RMSE improvement on rating prediction. The winning BellKor's Pragmatic Chaos solution was an ensemble blending matrix factorization (SVD variants) with restricted Boltzmann machines and hundreds of models; Netflix ultimately productionized the factorization and temporal-dynamics insights rather than the full ensemble, whose complexity outweighed its marginal gain — a lasting lesson about engineering pragmatism.",
        "Amazon's item-to-item collaborative filtering, described in its widely cited patent and IEEE paper, scaled recommendation by precomputing item-item similarity (co-purchase/co-view) offline rather than user-user similarity, which scales poorly with user count. At query time it simply looks up items similar to a user's recent interactions — an approach that remains a strong, cheap baseline two decades later.",
        "Collaborative filtering factorizes the sparse user-item interaction matrix into low-dimensional user and item embeddings whose dot product predicts affinity. Implicit-feedback variants (weighted ALS, BPR's pairwise ranking loss) handle the reality that most signals are clicks/views/plays rather than explicit ratings, and that unobserved interactions are missing-not-at-random rather than true negatives.",
        "YouTube's 2016 deep-learning recommender (Covington, Adams, Sargin) introduced a now-standard two-stage architecture: a candidate-generation network narrows millions of videos to hundreds using a deep network over user history embeddings framed as extreme multiclass classification, followed by a ranking network that scores candidates with richer features and an objective tuned to expected watch time rather than click probability.",
        "The two-tower (dual-encoder) model is the dominant retrieval architecture: a user/query tower and an item tower each produce an embedding, trained so relevant pairs have high dot-product similarity. Item embeddings are precomputed and indexed in an approximate-nearest-neighbor structure (HNSW, ScaNN, FAISS) so retrieval over hundreds of millions of items runs in milliseconds at serving time.",
        "A feature store (Feast and proprietary systems like Michelangelo, Zipline) solves train-serve skew by serving the same feature definitions and values offline for training and online for inference, with point-in-time-correct joins to avoid label leakage. It manages real-time features (recent activity), batch features, and the freshness/consistency guarantees that ranking quality depends on.",
        "Ranking models evolved from logistic regression and gradient-boosted trees to wide-and-deep and deep-and-cross networks that combine memorization of feature crosses with generalization, and increasingly to transformer-based sequence models (e.g., SASRec, BERT4Rec) that treat a user's interaction history as a sequence and predict the next item, capturing order and context that bag-of-history models miss.",
        "Online evaluation via A/B testing is the ground truth: offline metrics (AUC, NDCG, recall@k) guide development, but business metrics (engagement, retention, long-term value) are measured in controlled experiments. Practitioners must guard against feedback loops (the model shapes the data it is next trained on), popularity bias, filter-bubble effects, and the exploration-exploitation tradeoff that bandit and reinforcement-learning approaches address by occasionally surfacing uncertain items to gather signal.",
    ],
}

# Section themes used to frame the recombined domain paragraphs into a coherent
# long-form reference document. The same factual paragraphs recur under
# different analytical lenses — as they would in a real multi-section report —
# rather than being repeated verbatim back to back.
SECTION_THEMES = [
    "Executive Overview",
    "Historical Background and Timeline",
    "Structural and Technical Deep Dive",
    "Quantitative Data, Figures, and Reference Detail",
    "Comparative Analysis and Trade-offs",
    "Risk Factors and Open Questions",
    "Case Studies and Worked Examples",
    "Methodological Notes and Definitions",
    "Forward Outlook Through 2026 and Beyond",
    "Synthesis and Practitioner Takeaways",
]


def build_context(paragraphs: list[str], target_tokens: int) -> str:
    """Compose the curated domain paragraphs into a structured, coherent
    long-form document of ~`target_tokens` REAL tokens.

    Token-targeted (not char-targeted): the cl100k chars/token ratio varies by
    topic (number/acronym-dense prose tokenizes denser), so a fixed char target
    over/undershoots per topic. We sum real per-block token counts as we build
    and stop at the target — accurate within ~one paragraph regardless of topic.
    """
    out: list[str] = []
    total = 0  # running REAL token count
    section_idx = 0
    para_idx = 0
    while total < target_tokens:
        theme = SECTION_THEMES[section_idx % len(SECTION_THEMES)]
        pass_no = section_idx // len(SECTION_THEMES) + 1
        header = f"\n\n## Section {section_idx + 1} — {theme}"
        if pass_no > 1:
            header += f" (continued, part {pass_no})"
        header += "\n\n"
        out.append(header)
        total += count_tokens(header)
        # Each section emits a rotation of the paragraph pool with a short
        # connective lead-in that ties the paragraph to the section's lens.
        n_paras = len(paragraphs)
        for j in range(n_paras):
            p = paragraphs[para_idx % n_paras]
            para_idx += 1
            lead = f"{section_idx + 1}.{j + 1}  "
            block = lead + p + "\n\n"
            out.append(block)
            total += count_tokens(block)
            if total >= target_tokens:
                break
        section_idx += 1
    return "".join(out).rstrip() + "\n"


def extract_instruction(prompt: str) -> str:
    """Return the original instruction (idempotent): the text before the
    CONTEXT marker if the prompt is already padded, else the whole prompt."""
    if CONTEXT_MARKER.strip() in prompt:
        return prompt.split("--- CONTEXT ---", 1)[0].strip()
    return prompt.strip()


def uuid_prefix(instruction: str) -> str:
    """Extract the 8-char UUID prefix from a '[REQUEST-<uuid>] ...' instruction."""
    if instruction.startswith("[REQUEST-"):
        inner = instruction[len("[REQUEST-"):]
        return inner.split("-", 1)[0]
    return ""


def instruction_without_uuid(instruction: str) -> str:
    """Strip the leading '[REQUEST-uuid] ' so the instruction can be repeated
    cleanly after the context block."""
    if instruction.startswith("[REQUEST-") and "]" in instruction:
        return instruction.split("]", 1)[1].strip()
    return instruction


def pad_prompt(instruction: str) -> str:
    prefix = uuid_prefix(instruction)
    paragraphs = SEEDS.get(prefix)
    if not paragraphs:
        raise KeyError(
            f"No seed domain content for UUID prefix '{prefix}'. "
            f"Instruction: {instruction[:80]!r}"
        )
    body = build_context(paragraphs, BODY_TARGET_TOKENS)
    tail = instruction_without_uuid(instruction)
    return (
        f"{instruction}"
        f"{CONTEXT_MARKER}"
        f"{body}"
        f"{END_MARKER}"
        f"Based on the context above, {tail[0].lower()}{tail[1:]}"
        if tail else
        f"{instruction}{CONTEXT_MARKER}{body}{END_MARKER}Based on the context above, respond to the request."
    )


def count_tokens(text: str) -> int:
    """Real cl100k token count via tiktoken if installed (no hard dep — the
    runner may not have it), else the calibrated chars/token fallback so the
    estimate still reflects REAL tokens rather than the old len//4 over-count."""
    try:
        import tiktoken
        return len(tiktoken.get_encoding("cl100k_base").encode(text))
    except Exception:
        return int(len(text) / CHARS_PER_TOKEN)


def main() -> None:
    src = json.loads(SRC.read_text())
    originals = src["prompts"]
    print(f"Loaded {len(originals)} prompts from {SRC.name}")
    print(f"\n{'#':>3}  {'orig tok':>9}  {'padded tok':>11}  {'target met':>10}")
    print("-" * 42)

    padded_prompts: list[str] = []
    for i, raw in enumerate(originals):
        instruction = extract_instruction(raw)
        padded = pad_prompt(instruction)
        padded_prompts.append(padded)
        orig_tok = count_tokens(instruction)
        pad_tok = count_tokens(padded)
        met = "yes" if 14_000 <= pad_tok <= 17_000 else "NO"
        print(f"{i + 1:>3}  {orig_tok:>9}  {pad_tok:>11}  {met:>10}")

    out = {
        "version": "v2-padded",
        "count": len(padded_prompts),
        "target_tokens_per_prompt": TARGET_TOKENS,
        "prompts": padded_prompts,
    }
    PADDED.write_text(json.dumps(out, indent=2, ensure_ascii=False))
    print(f"\nWrote padded dataset -> {PADDED}")
    # Overwrite the canonical file so s02_long_context.py (which calls
    # load_prompts("long_context_v2.json")) picks it up WITHOUT modification.
    SRC.write_text(json.dumps(out, indent=2, ensure_ascii=False))
    print(f"Overwrote canonical    -> {SRC} (s02 loads this unmodified)")

    validate(PADDED)
    validate(SRC)


def validate(path: Path) -> None:
    data = json.loads(Path(path).read_text())
    assert data["count"] == 10, "Must have exactly 10 prompts"
    results = []
    for i, p in enumerate(data["prompts"]):
        tokens = count_tokens(p)
        ok = 14_000 <= tokens <= 17_000
        # extra integrity checks the spec requires
        has_uuid = p.startswith("[REQUEST-")
        has_repeat = "--- END CONTEXT ---" in p and "Based on the context above," in p
        results.append((i + 1, tokens, "PASS" if (ok and has_uuid and has_repeat) else "FAIL"))
    print(f"\nValidation results for {Path(path).name}:")
    print(f"{'#':>3}  {'tokens':>8}  {'status'}")
    print("-" * 25)
    for row in results:
        print(f"{row[0]:>3}  {row[1]:>8}  {row[2]}")
    fails = [r for r in results if r[2] == "FAIL"]
    if fails:
        raise AssertionError(f"{len(fails)} prompts outside token range / missing structure — see above")
    print("\nAll 10 prompts validated. Dataset ready for S-02.")


if __name__ == "__main__":
    main()
