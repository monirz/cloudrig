# FROM scratch so the build pulls nothing: the binary is compiled outside and
# copied in. See the Cloud Run section of the README for the two commands.
FROM scratch
COPY server /server
ENTRYPOINT ["/server"]
