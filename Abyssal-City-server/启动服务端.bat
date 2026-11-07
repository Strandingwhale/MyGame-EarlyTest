@echo off
chcp 65001 >nul
setlocal
cd /d "%~dp0"

echo ==========================================================
echo   无底深城 服务端 一键启动
echo   启动顺序: datasave(9997) - login(9999) - battle(9998) - gateway(8888)
echo ==========================================================
echo.

if not exist "bin\gateway.exe" (
    echo [错误] 找不到 bin\gateway.exe
    echo        请把本脚本放在 Abyssal-City-server 根目录下运行。
    pause
    exit /b 1
)

echo [1/4] datasave 9997 ...
start "无底深城-datasave" bin\datasave.exe
timeout /t 2 /nobreak >nul

echo [2/4] login 9999 ...
start "无底深城-login" bin\login.exe
timeout /t 2 /nobreak >nul

echo [3/4] battle 9998 ...
start "无底深城-battle" bin\battle.exe
timeout /t 2 /nobreak >nul

echo [4/4] gateway 8888 ...
start "无底深城-gateway" bin\gateway.exe
timeout /t 3 /nobreak >nul

echo.
echo ---- 端口监听检查 ----
netstat -ano | findstr LISTENING | findstr ":8888 :9999 :9998 :9997"
echo.
echo 四个服务已拉起。现在可以双击游戏 exe 开始玩。
echo 前置条件: MySQL(3306) 与 Redis(6379) 必须先运行。
echo 关闭服务: 直接关掉 4 个黑色命令行窗口，或运行 停止服务端.bat
echo.
pause
