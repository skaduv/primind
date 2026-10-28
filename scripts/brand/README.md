# 平台标识（Logo）资源

平台所有对外可见的品牌图形都从这里产出。主标识是「RISE 全域数智化平台」，
原始文件为白底 PNG：`scripts/brand/source/rise-logo-master.png`。

## 一、产物清单

| 文件 | 尺寸 | 用途 |
| --- | --- | --- |
| `frontend/src/assets/brand/rise-logo.png` | 800×296 | 完整标识（图形 + RISE + 副标题），登录页等大面积场景 |
| `frontend/src/assets/brand/rise-logo-on-dark.png` | 800×296 | 深色背景版（文字转浅色，图形与渐变条保留） |
| `frontend/src/assets/brand/rise-logo-compact.png` | 480×178 | 紧凑版（图形 + RISE，无副标题），侧栏 |
| `frontend/src/assets/brand/rise-logo-compact-on-dark.png` | 480×178 | 紧凑版深色背景版 |
| `frontend/src/assets/brand/rise-mark.png` | 512×512 | 纯图形，正方形透明画布，供后续扩展使用 |
| `frontend/public/favicon.ico` | 16–256 多尺寸 | 浏览器页签 |
| `frontend/public/apple-touch-icon.png` | 180×180 | iOS 添加到主屏 |
| `cmd/desktop/build/appicon.png` | 1024×1024 | 桌面端（Wails）应用图标 |

## 二、代码中的引用位置

- `frontend/src/components/menu.vue`：左侧栏顶部 logo，按 `theme-mode` 切换深浅两套。
- `frontend/src/views/auth/Login.vue`：登录页左上角 logo，放在白色圆角底上（登录页是深绿渐变背景）。
- `frontend/index.html`、`frontend/embed.html`：`favicon.ico` / `apple-touch-icon.png`。
- `cmd/desktop/build/appicon.png`：Wails 打包时自动转成 exe / icns / ico。

## 三、更换或调整标识

1. 用新的白底主标识覆盖 `scripts/brand/source/rise-logo-master.png`；
2. 如果新图的内容边界变了，调整脚本顶部的 `FULL_BOX`、`MARK_BOX`（以及深色版里
   `GRADIENT_BAR_BOX` 渐变条的位置）；
3. 运行：

   ```bash
   python scripts/brand/build_logo_assets.py
   ```

脚本会一次性重写上面所有产物，不需要手工改图或逐个导出。

## 四、处理约定

- **去白底**：用 color-to-alpha + 反预乘，避免深色背景下出现白色描边。
- **缩放**：预乘 alpha + BOX 采样，避免 LANCZOS 在透明边缘产生振铃/锯齿。
- **深色版**：只把深蓝色文字与副标题改成浅色，左侧图形和 “E” 上的蓝绿渐变条保持原样；
  不要用 CSS `filter: invert()` 整体反色。
- **登录页**：深绿渐变背景上彩色标识对比度不足，因此放在白色圆角底（`.logo-chip`）里；
  登录页不随主题切换，始终使用彩色版。
- **侧栏尺寸**：logo 宽度 100px，行高 39px；改宽度时注意别超过行高，否则会被裁切。
