# Records the README's demo GIF: `vhs demo/demo.tape` from the repo root.
# Real time throughout, so the stopwatch on screen is the one a reader gets.
Output demo/demo.gif

Require docker
Require kind
Require kubectl
Require helm
Require jq

Set Shell bash
Set FontSize 14
Set Width 1280
Set Height 860
Set Padding 20
Set TypingSpeed 60ms

Env PAUSE 8

Type "./demo/run.sh"
Sleep 500ms
Enter
Wait+Screen@4m /Your knowledge isn't/
Sleep 4s
