# promo — CoSpace 宣传视频

62 秒 1920×1080 动效视频，中文文案，叙事顺序遵循 DESIGN.md 的对外宣传优先级。Host 段和控制台段用的是真实的 Web 控制台与邀请页，配演示数据；画面里不出现任何真实域名或成员信息。

运行环境：macOS 本机，全局 npm `playwright` + ffmpeg + python3/numpy。

- `capture.cjs`：用 mock API 打开 `internal/api/web/index.html` 和 `internal/api/invite.html`，截出各状态的界面图和点击坐标，输出到 `ui/`。
- `index.html`：全部画面与时间轴（`window.__render(t)` 确定性渲染）。浏览器直接打开即循环预览，`?t=12.3` 定格某一帧。
- `music.py`：合成配乐（FM 电钢琴 + 贝斯 + 鼓 + 点击音效），与画面切点对齐。
- `render.cjs`：逐帧截图并用 ffmpeg 合成 `out/cospace-promo.mp4`；`--stills 4,18,29` 只出单帧 PNG 用于检查。

```bash
NODE_PATH=$(npm root -g) node capture.cjs
python3 music.py out/music-raw.wav
ffmpeg -y -i out/music-raw.wav -af loudnorm=I=-16:TP=-1.5:LRA=9 -ar 44100 out/music.wav
NODE_PATH=$(npm root -g) node render.cjs
```

`ui/` 与 `out/` 为生成物，不入库。改了控制台或邀请页 UI 后重跑 `capture.cjs`。
