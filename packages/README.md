# packages/

Optional offline build inputs for the Docker images.

`docker/Dockerfile.docreader` installs the pinned `protoc` release for the
docreader image. If a `protoc-<version>-linux-<arch>.zip` archive is present in
this directory it is used for an offline install; otherwise the image build
downloads the archive from the official protobuf release URL. Only the Linux
protoc archives are whitelisted in `.dockerignore`.

The archives themselves are not committed (see `.gitignore`); this directory is
tracked so `COPY packages/ ...` in the Dockerfiles always has a source path.
