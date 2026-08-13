# aglink-screen

[agentlink](https://github.com/tyranno/aglink-screen) 계열의 첫 플러그인 —
LLM 에이전트가 Windows 화면(UIA/Win32/GDI)을 직접 조작하게 해주는 독립 실행파일.

원래 [teleclaude](https://github.com/tyranno/teleclaude)에 `__mcp-screen`이라는
숨은 서브커맨드로 내장돼 있던 화면제어 기능을 별도 프로젝트로 분리한 것.
teleclaude 본체("대화 감독" — 라우팅/스케줄러/텔레그램)는 이 실행파일을 자식
프로세스로 호출만 하고, 실제 화면 조작 로직은 여기 전부 들어있다.

## 빠른 시작 (일반 MCP 클라이언트)

**Windows 전용.** 어떤 MCP 클라이언트(Claude Code CLI, Claude Desktop, Cursor 등)에든
stdio MCP 서버로 등록해서 씁니다.

1. **바이너리 준비** — [Releases](https://github.com/tyranno/aglink-screen/releases)에서
   `aglink-screen.exe`를 받거나, 소스에서 빌드:
   ```powershell
   go build -o aglink-screen.exe .
   ```

2. **MCP 서버로 등록** — 실행 파일을 `mcp` 인자로 띄우면 stdio MCP 서버가 됩니다
   (`mcp`가 기본값이라 인자를 생략해도 됨).
   - **Claude Code CLI**
     ```powershell
     claude mcp add screen -- "C:\path\to\aglink-screen.exe" mcp
     ```
   - **Claude Desktop / 일반 MCP 클라이언트** — 설정의 `mcpServers`에 추가:
     ```json
     {
       "mcpServers": {
         "screen": {
           "command": "C:\\path\\to\\aglink-screen.exe",
           "args": ["mcp"]
         }
       }
     }
     ```

3. **(선택) 관리자 권한** — "관리자 권한으로 실행"된 앱을 클릭/입력하려면(Windows UIPI)
   MCP 클라이언트 자체를 관리자 권한으로 실행해야 합니다.

### 사용법
등록 후 에이전트에게 자연어로 시키면 됩니다:
```
메모장 열어서 "안녕"이라고 쓰고 저장해줘
지금 열려있는 창 목록 보여줘
계산기에서 12 곱하기 34 눌러줘
```
에이전트는 먼저 `snapshot`(UIA 요소 트리)으로 화면을 읽고, 좌표 없이
`invoke`/`set_value`/`click_control`로 조작합니다. 창 내용은 `get_text`/`get_value`로
읽고, 스크린샷(비전)은 최후 수단입니다.

---

- **UIA 우선** — `snapshot`/`invoke`/`set_value`/`get_value`로 대부분의 네이티브 앱을 좌표 없이 조작
- **Win32 자식창 폴백** — `win_controls`/`click_control`, UIA가 비어도 정확한 좌표 확보
- **GDI 캡처** — `screenshot`/`capture_window`/`capture_region`, 비전 다운스케일을 피해 정확한 좌표 매핑
- **입력** — `click`/`double_click`/`triple_click`/`drag`/`move`/`type`/`key`(hold_ms 지원)/`scroll` (+ modifier 조합), `get_cursor_position`으로 현재 좌표 확인
- **가상 데스크톱 인식** — `focus_window`/`return_desktop`이 데스크톱 경계를 넘나듦
- **대기 프리미티브** — `wait_for_window`(창), `wait_for_control`(UIA 요소), 뜰 때까지 수동 폴링할 필요 없음
- **창 배치** — `move_window`(정확한 좌표/크기), `window_state`(최소화/최대화/복원), `get_window_rect`(현재 위치/크기/상태 확인), `close_window`(특정 창을 정확히 지정해서 닫기 — foreground에 의존하는 `key("alt+f4")`보다 안전)
- **좌표 프리셋** — `preset_save`/`preset_click`/`preset_list`
- **관리자 권한 대상 앱** — UIPI 감지 + 경고 (`screen_control.elevated`로 우회)

Windows 전용 (`GOOS=windows` 빌드 태그). 다른 OS에서는 스텁이 명확한 에러를 반환한다.

## 실행 모드

```
aglink-screen              # 기본값. MCP stdio 서버로 기동 (아래 "mcp"와 동일)
aglink-screen mcp          # 명시적으로 같음
aglink-screen cmd <sub> [args...] [--presets <path>]
                            # LLM 우회 fast-path. 결과를 JSON으로 stdout에 출력:
                            #   {"text": "...", "image": "<base64 PNG, 있으면>", "error": "..."}
```

`cmd`의 서브커맨드: `list` (창 목록) · `shot [창이름]` (스크린샷) ·
`region <x> <y> <w> <h> [창이름]` (영역 캡처) · `preset save <이름>` ·
`click <프리셋이름>`.

## teleclaude와 연결

teleclaude는 `screen_control.binary_path`(config.yaml)로 이 실행파일 경로를
찾는다. 값이 비어 있으면 teleclaude 실행파일과 **같은 폴더**에서
`aglink-screen(.exe)`를 찾는다 — 배포 시 두 실행파일을 나란히 두면 별도 설정
없이 동작한다.

```yaml
screen_control:
  enabled: true
  binary_path: ""   # 비우면 teleclaude exe와 같은 폴더에서 자동 탐색
  elevated: false
  keep_awake: false
```

teleclaude 쪽에서는 워커의 `--mcp-config`가 `aglink-screen mcp`를 가리키게
하고, `!screen` 텔레그램 명령은 `aglink-screen cmd ...`를 서브프로세스로
실행해 JSON 결과를 파싱한다.

### 제어권(Control Ownership)

대화(worker)마다 별도 aglink-screen 프로세스가 뜨고 **같은 물리 화면 하나**를
조작하므로, 두 대화가 동시에 입력을 합성하면 충돌한다. 이를 막기 위한
크로스-프로세스 제어권 조율(리스 + 세션-로컬 뮤텍스, fail-fast `SCREEN_BUSY`
응답, `control_status` 사전 확인 툴)의 **설계와 MCP 계약**은
[docs/control-ownership.md](docs/control-ownership.md)에 정리돼 있다 —
teleclaude 호출측 개발은 이 문서를 참조.

### 통합 배포 (`!update`)

teleclaude와 이 저장소를 **형제 디렉터리**(예: `..\teleclaude`, `..\aglink-screen`)로
나란히 clone해두면, teleclaude의 텔레그램 `!update` 명령이 teleclaude 자체를
빌드하기 전에 이 저장소도 함께 `go build`해서 teleclaude 실행파일 옆에
떨어뜨려준다 — 저장소 두 개를 각각 손으로 빌드/복사할 필요 없이 `!update`
한 번으로 둘 다 최신화된다. 형제 디렉터리가 없으면(화면제어가 필요 없는
헤드리스 배포 등) 조용히 건너뛴다 — 자세한 내용은
[teleclaude README의 "플러그인 확장" 절](https://github.com/tyranno/teleclaude#플러그인-확장-aglink-)
참고.

## 빌드

```powershell
go build -o aglink-screen.exe .
```

teleclaude와 같은 폴더에 두면(예: `..\Teleclaude\aglink-screen.exe`) 별도
설정 없이 바로 인식된다.

## 관리자 권한 대상 앱

대상 앱이 관리자(High integrity)로 떠 있으면 Windows UIPI가 일반 권한 프로세스의
합성 입력(클릭 등)을 무음 차단한다. `click_control`/`invoke` 결과에 UIPI 경고가
붙으면, teleclaude 쪽 `screen_control.elevated: true`로 전체 프로세스 체인
(teleclaude → claude worker → aglink-screen)을 관리자 권한으로 재기동해야 한다.
