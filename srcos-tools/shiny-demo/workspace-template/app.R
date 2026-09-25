# Shiny 应用：首次使用时由 SRCOS 从 tool.yaml 的 workspace.init_from 拷进
# /workspace。之后它就是**你的**文件 —— 改完重启实例即可看到效果。

library(shiny)

# 工具的 interface 参数以 SRCOS_PARAM_<大写名> 注入；管理端启动不带参数时
# 回落到 app.R 里的默认值。
default_title <- "SRCOS Shiny Demo"
title <- Sys.getenv("SRCOS_PARAM_TITLE", default_title)
if (identical(title, "")) title <- default_title

ui <- fluidPage(
  titlePanel(title),
  sidebarLayout(
    sidebarPanel(
      sliderInput("n", "样本数", min = 5, max = 200, value = 40),
      selectInput("dist", "分布", c("正态" = "norm", "均匀" = "unif")),
      helpText("这个实例跑在 SRCOS 的沙箱里：只有一个工作区（/workspace）可写，",
               "端口由平台分配。")
    ),
    mainPanel(
      plotOutput("hist"),
      verbatimTextOutput("who")
    )
  )
)

server <- function(input, output, session) {
  samples <- reactive({
    switch(input$dist,
      norm = rnorm(input$n),
      unif = runif(input$n)
    )
  })

  output$hist <- renderPlot({
    hist(samples(),
      main = paste("n =", input$n, "·", input$dist),
      col = "#4F46E5", border = "white", xlab = "value")
  })

  # 服务实例的身份对工具可见：SRCOS_USER / SRCOS_TOOL / SRCOS_INSTANCE_ID。
  output$who <- renderText({
    paste0(
      "user: ", Sys.getenv("SRCOS_USER", "?"),
      " · tool: ", Sys.getenv("SRCOS_TOOL", "?"), " v", Sys.getenv("SRCOS_TOOL_VERSION", "?"),
      " · instance: ", Sys.getenv("SRCOS_INSTANCE_ID", "?"),
      " · pid: ", Sys.getpid()
    )
  })
}

shinyApp(ui, server)
