# Preparing a GitHub release

Publishing remains a separate operator action.
The source package targets `daviestechlabs/research-assistant`.
Create that repository before starting this procedure.

1. Clone the repository into a clean directory.
2. Copy the standalone package into that directory, including `.github`, `.gitignore`, and `.dockerignore`.
3. Keep the clone's `.git` directory.
4. Run `python3 scripts/check-public.py` before staging files.
5. Run the Go and Helm checks from the README.
6. Inspect `git diff --cached` before creating a commit.
7. Push the reviewed commit to the repository.
8. Confirm both GitHub CI jobs pass.

The container workflow builds an image and checks its PDF tools.
It does not publish an image or use registry credentials.
Build and publish a registry image separately when the repository is ready.
Update deployment values with the resulting immutable digest.
The examples deliberately contain a placeholder image and remain suspended.

The supplied source archive has a file manifest and SHA-256 checksums.
Helm chart packages contain the same generated application bundle and CRD.
No private deployment configuration belongs in a release commit.
