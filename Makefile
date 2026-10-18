# Avoid Git Bash rewriting cmd's /c flag into a Windows path.
SHELL := pwsh.exe

run:
	node tools\build.mjs && build\mirrorbird.exe

run-without-build:
	build\mirrorbird.exe