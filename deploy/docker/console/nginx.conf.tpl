# Nexus Console — nginx config template for the container form factor.
# Derived from nexus-ami/artifacts/configs/nginx-nexus.conf; the control-plane
# runs INSIDE this container (127.0.0.1:3001), while the AI Gateway and Hub
# upstreams are env-parameterized service names rendered by the entrypoint.
# Keep the two files' location blocks in lockstep when either changes.

server {
    listen      80 default_server;
    server_name _;
    return      301 https://$host$request_uri;
}

server {
    listen              443 ssl default_server;
    server_name         _;

    ssl_certificate     /etc/nexus/tls.crt;
    ssl_certificate_key /etc/nexus/tls.key;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;

    root                /opt/nexus/ui;
    index               index.html;

    client_max_body_size 32m;

    # Vite SPA fallback — every unmatched path serves index.html so the
    # client-side router takes over.
    location / {
        try_files $uri $uri/ /index.html;
    }

    # Admin API + auth-server endpoints (both live in the in-container
    # control-plane on :3001). proxy_buffering off so the admin SSE surfaces
    # stream instead of buffering into one late chunk.
    location /api/ {
        proxy_pass         http://127.0.0.1:3001;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_buffering    off;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    # SCIM 2.0 provisioning (Okta / Entra ID push user+group sync).
    location /scim/ {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
    }

    # AI Gateway ingress — one regex block covers every ingress wire format
    # (/v1 OpenAI canonical, /v1beta Gemini, /openai/deployments Azure,
    # /api/paas GLM). Regex so /api/paas wins over the plain /api/ prefix.
    # proxy_buffering off + HTTP/1.1 so SSE streams chunk-by-chunk.
    location ~ ^/(v1|v1beta|openai/deployments|api/paas)(/|$) {
        proxy_pass         ${NEXUS_AI_GATEWAY_UPSTREAM};
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_buffering    off;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }

    # Nexus Hub — remote endpoint-agent connectivity only (WebSocket +
    # enrollment/thingclient HTTP fallback). Bearer-token gated by the Hub.
    location /ws {
        proxy_pass         ${NEXUS_HUB_UPSTREAM};
        proxy_http_version 1.1;
        proxy_set_header   Upgrade           $http_upgrade;
        proxy_set_header   Connection        "upgrade";
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    location /api/internal/things/ {
        proxy_pass         ${NEXUS_HUB_UPSTREAM};
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    location /.well-known/ {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
    }

    # OAuth/OIDC auth-server endpoints — without this block /oauth/authorize
    # falls through to the SPA try_files handler and the login flow loops.
    location /oauth/ {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
    }

    # Auth-server pre-bearer endpoints (IdP list, password login). Without the
    # proxy they return SPA HTML and the login page shows
    # "Unable to load sign-in methods".
    location /authserver/ {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
    }

    # Health + readiness for LBs / orchestrators — served by the control-plane
    # at the ROOT (/healthz, /ready), so proxy_pass omits a URI.
    location = /healthz {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
    }

    location = /ready {
        proxy_pass         http://127.0.0.1:3001;
        proxy_set_header   Host              $host;
    }
}
