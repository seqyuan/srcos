"""SRCOS · Shiny for Python —— 交互输入 → 文本输出。

放到 workspace-template/ 里，首次使用时会拷进用户的 workspace，
之后用户可以在自己的 workspace 里改这份 app.py。
"""
import os

from shiny import App, ui, render

app_ui = ui.page_fluid(
    ui.h2(os.environ.get("SRCOS_PARAM_TITLE", "SRCOS · Shiny for Python")),
    ui.input_text("name", "你的名字", value="world"),
    ui.input_slider("times", "重复次数", min=1, max=5, value=2),
    ui.output_text_verbatim("greeting"),
)

def server(input, output, session):
    @render.text
    def greeting():
        return "\n".join(f"你好，{input.name()}！(第 {i} 行)" for i in range(1, input.times() + 1))

app = App(app_ui, server)
