# Manual Setup: Wilcom Engine Wrapper

Handles .emb previews and conversions via Wine.

## Setup & Run
1. **Prerequisites**: Install Wine and Xvfb on your system.
   ```bash
   sudo apt update
   sudo apt install -y xvfb wine64 winetricks
   ```
2. **Setup Virtual Environment**:
   ```bash
   python3 -m venv .venv
   source .venv/bin/activate
   ```
3. **Install Dependencies**:
   ```bash
   pip install -r requirements.txt
   ```
4. **Run Service**:
   ```bash
   python3 server.py
   ```

> [!NOTE]
> This service is optional and only required for `.emb` preview rendering.

## Configuration
The service expects ES.EXE or TrueSizer.exe to be installed in your Wine C: drive under Program Files/Wilcom/.
