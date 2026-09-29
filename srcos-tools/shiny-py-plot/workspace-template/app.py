"""SRCOS · Shiny for Python —— 参数 → 直方图（matplotlib）。"""
import numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import os

from shiny import App, ui, render

app_ui = ui.page_fluid(
    ui.h2(os.environ.get("SRCOS_PARAM_TITLE", "SRCOS · Shiny for Python 绘图")),
    ui.layout_sidebar(
        ui.sidebar(
            ui.input_slider("n", "样本数", min=10, max=2000, value=300),
            ui.input_select("dist", "分布", {"norm": "正态", "unif": "均匀", "exp": "指数"}),
            ui.input_slider("bins", "分箱数", min=5, max=80, value=30),
        ),
        ui.output_plot("hist"),
    ),
)

def server(input, output, session):
    @render.plot
    def hist():
        rng = np.random.default_rng(0)
        n = input.n()
        dist = input.dist()
        if dist == "norm":
            x = rng.normal(size=n)
        elif dist == "unif":
            x = rng.uniform(0, 1, size=n)
        else:
            x = rng.exponential(size=n)
        fig, ax = plt.subplots(figsize=(6, 4))
        ax.hist(x, bins=input.bins())
        ax.set_title(f"{dist}  n={n}")
        ax.set_xlabel("value")
        ax.set_ylabel("count")
        fig.tight_layout()
        return fig

app = App(app_ui, server)
