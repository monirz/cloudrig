# A CI failure, then the same failure on a laptop from one snapshot file.
#
# Regenerate with `make demo-replay`. As with demo.tape, the tape only
# captures frames and the Makefile encodes the GIF.

Output .demoframes/

Set Shell "bash"
Set FontSize 16
Set Width 1100
Set Height 720
Set Padding 24
Set TypingSpeed 30ms
Set Theme "Catppuccin Mocha"

# Off camera: the binary on PATH, the example's module, the emulator variables,
# and the test compiled once so the recording shows the run, not the build.
Hide
Type "export PATH=$PWD:$PATH PS1='$ ' GOWORK=off STORAGE_EMULATOR_HOST=localhost:4599 PUBSUB_EMULATOR_HOST=localhost:4599; set +m; pkill -f 'cloudrig start'; cd examples/ci-replay; rm -f start.tar; go test -count=1 -run '^$' ./... >/dev/null; clear"
Enter
Sleep 8s
Show

Type "# In CI: make Publish slow, snapshot, then test"
Enter
Type "(cloudrig start >/dev/null &)"
Enter
Sleep 2s
Type "cloudrig fault pubsub --path /google.pubsub.v1.Publisher/Publish --latency 3s --failure-rate 20%"
Enter
Sleep 1s
Type "cloudrig snapshot start.tar"
Enter
Sleep 1s
Type "go test ./..."
Enter
Sleep 10s

Type "# Later, on a laptop: a fresh emulator and the same file"
Enter
Type "pkill -f 'cloudrig start'; (cloudrig start >/dev/null &)"
Enter
Sleep 2s
Type "cloudrig restore start.tar"
Enter
Sleep 1s
Type "go test ./..."
Enter
Sleep 12s

Hide
Type "pkill -f 'cloudrig start'; rm -f start.tar"
Enter
Show
