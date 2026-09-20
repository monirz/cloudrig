# CloudRig in 30 seconds: its own CLI, then unmodified gcloud.
#
# Regenerate with `make demo`. The tape only captures frames — this vhs and
# ffmpeg build disagree about the GIF encode, so the Makefile does that step.

Output .demoframes/

Set Shell "bash"
Set FontSize 17
Set Width 1100
Set Height 560
Set Padding 24
Set TypingSpeed 35ms
Set Theme "Catppuccin Mocha"

# Off camera: put the built binary on PATH so the recording shows `cloudrig`
# rather than `./cloudrig`, quieten the prompt, and turn off job control so
# backgrounding does not print a job number.
Hide
Type "export PATH=$PWD:$PATH PS1='$ '; set +m; pkill -f 'cloudrig start'; clear"
Enter
Sleep 1s
Show

Type "cloudrig start &"
Enter
Sleep 3s
# A fresh prompt, so the daemon's startup lines do not race the next command.
Enter
Sleep 500ms

Type "cloudrig fn deploy hello --source ./testdata/go-hello"
Enter
Sleep 4s

Type "curl localhost:4599/us-central1-cloudrig-local/hello"
Enter
Sleep 3s

# The same emulator, now through gcloud with no project and no credentials.
Type ". ./cloudrig-env.sh"
Enter
Sleep 2s

Type "gcloud functions list"
Enter
Sleep 4s

Type "gcloud storage buckets create gs://demo"
Enter
Sleep 3s

Type "gcloud storage cp README.md gs://demo/"
Enter
Sleep 3s

Type "gcloud storage ls gs://demo"
Enter
Sleep 5s

Hide
Type "pkill -f 'cloudrig start'"
Enter
Show
