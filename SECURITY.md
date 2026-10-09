# Security

Keep credentials in environment variables or Kubernetes Secrets.
The app sends its chat key only to its configured chat endpoint.
LiteLLM mode also requires an explicitly trusted gateway origin.
Redirects cannot move model credentials or storage requests to another host.

Loopback previews permit local access.
Remote serving requires HTTPS, an OIDC issuer, an audience, and an allowed-subject list.
Private PDFs and sessions require authentication.
Protect public model access with LiteLLM limits and a scoped service key.

Source text remains untrusted data.
Evidence reviews can make mistakes despite structural and excerpt checks.
Do not submit credentials or private documents in public issue reports.
