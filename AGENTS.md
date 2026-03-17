# Releasing

To release a new Docker image on GHCR, tag the commit with a semver tag and push:

```
git tag v0.X.Y && git push origin main v0.X.Y
```

The GitHub Actions workflow builds and pushes `ghcr.io/skorokithakis/havpe-server` with the version tag and `latest`.
