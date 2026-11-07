@echo off
chcp 65001 >nul
echo 正在关闭无底深城服务端...
taskkill /FI "WINDOWTITLE eq 无底深城-gateway*" /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq 无底深城-battle*" /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq 无底深城-login*" /F >nul 2>&1
taskkill /FI "WINDOWTITLE eq 无底深城-datasave*" /F >nul 2>&1
echo 已发送关闭指令。
pause
