# FROM scratch so the build pulls nothing: the binary is built outside and
# copied in. build.sh compiles it for the daemon's architecture.
FROM scratch
COPY server /server
ENTRYPOINT ["/server"]
