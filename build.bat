@echo off
cd /d %~dp0
set GOOS=linux
set GOARCH=arm
set GOARM=7
go build
scp ./roombaGo pizero2@pizero2.local:/home/pizero2