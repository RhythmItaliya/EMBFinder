@echo off
setlocal enabledelayedexpansion
title EMBFinder - Windows Launcher

echo =======================================================
echo           EMBFinder - Visual Search Engine
echo =======================================================
echo.

set ROOT=%cd%
set GO_SERVER_DIR=%ROOT%\go-server
set EMBEDDER_DIR=%ROOT%\embedder
set EMB_ENGINE_DIR=%ROOT%\emb-engine

:: Check for Python
python --version >nul 2>&1
if %errorlevel% neq 0 (
    echo [ERROR] Python is not installed or not in PATH.
    echo Please install Python 3.10+ from python.org
    pause
    exit /b 1
)

:: Check for Go
go version >nul 2>&1
if %errorlevel% neq 0 (
    echo [ERROR] Go is not installed or not in PATH.
    echo Please install Go 1.22+ from golang.org
    pause
    exit /b 1
)

:: ---------------------------------------------------------
:: Setup EMB-Engine VENV
:: ---------------------------------------------------------
echo [1/3] Setting up EMB Engine...
cd "%EMB_ENGINE_DIR%"
if not exist ".venv\" (
    echo Creating virtual environment for emb-engine...
    python -m venv .venv
)
call .venv\Scripts\activate.bat
echo Installing dependencies...
pip install -r requirements.txt >nul
start "EMB Engine (8767)" cmd /c "title EMB Engine && call .venv\Scripts\activate.bat && python server.py"
call .venv\Scripts\deactivate.bat

:: ---------------------------------------------------------
:: Setup Embedder VENV
:: ---------------------------------------------------------
echo [2/3] Setting up AI Embedder...
cd "%EMBEDDER_DIR%"
if not exist ".venv\" (
    echo Creating virtual environment for embedder...
    python -m venv .venv
)
call .venv\Scripts\activate.bat
echo Installing dependencies...
pip install -r requirements.txt >nul
start "AI Embedder (8766)" cmd /c "title AI Embedder && call .venv\Scripts\activate.bat && uvicorn main:app --port 8766"
call .venv\Scripts\deactivate.bat

:: ---------------------------------------------------------
:: Start Go Server
:: ---------------------------------------------------------
echo [3/3] Starting Go Server...
cd "%GO_SERVER_DIR%"
echo Compiling and running Go backend...
set MODE=development
set HEADLESS=0

:: We run the dev build which includes the Wails desktop window if configured
:: Or it will just launch the headless server and open the browser.
start "Go Server (8765)" cmd /c "title Go Server && go run --tags dev ."

echo.
echo All services have been launched in separate windows!
echo Web UI:       http://127.0.0.1:8765
echo Embedder:     http://127.0.0.1:8766/health
echo EMB Engine:   http://127.0.0.1:8767/health
echo.
echo Keep this window open. Close the other windows to stop the servers.
pause
