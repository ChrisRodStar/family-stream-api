# Family Stream API

Go client-facing API for web and iOS. Own authentication, profiles, personal libraries, watch history/progress, and playback coordination. Expose one public contract with multiple functional routes through the private Tailscale address. Catalog, providers, and relay use private, versioned contracts.

## Data and playback boundaries

Keep household state in API-owned persistent storage. Use targeted resume/progress lookups rather than retrieving the entire library for one title. Keep session cleanup out of the frequent progress-update path; batch or schedule maintenance.

Read catalog data through the catalog contract and retain a last-known-good read snapshot so cached browsing survives a catalog process outage. Do not open the catalog's SQLite file. Identify titles by media type and TMDB ID, retain external IDs, and send known identifiers and episode coordinates in provider requests without requiring a fresh catalog call for playback.

Authorize playback, request candidate sources, establish relay session/source state, and return an authorized `/stream/*` URL. The relay validates authorization and serves media directly through Tailscale Serve. Do not proxy segments through the API process or expose provider URLs to clients. Define recoverable session state and token expiry/renewal in the contracts.

## Deployment

Package the API with independently built React/Vite assets in the application container; the API serves both the client routes and static assets. Keep web release activation separate from API process replacement. Application egress stays outside Mullvad. Provider/relay source egress belongs to the streaming container's VPN policy.

This project is a documentation scaffold. Implement against the agreed identifiers and proven internal contracts. Tests, builds, benchmarks, and deployments require an explicit user request.
