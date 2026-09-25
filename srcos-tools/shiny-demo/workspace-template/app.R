# Shiny 应用：首次使用时由 SRCOS 从 tool.yaml 的 workspace.init_from 拷进
# /workspace。之后它就是**你的**文件 —— 改完重启实例即可看到效果。

library(shiny)

# 工具的 interface 参数以 SRCOS_PARAM_<大写名> 注入；管理端启动不带参数时
# 回落到 app.R 里的默认值。
default_title <- "SRCOS Shiny Demo"
title <- Sys.getenv("SRCOS_PARAM_TITLE", default_title)
if (identical(title, "")) title <- default_title

# ── 托管 agent 的实例身份（ADR-025）───────────────────────────────────────
# 工具的 tool.yaml 声明了 `agent: { mcp: [read] }`，于是 SRCOS 在实例启动时
# 签发一枚**只读**的实例凭据，放在 $HOME/.srcos/agent-token。这里用它调
# SRCOS 自己的只读 API（SRCOS_API）—— 这正是「托管 agent」的最小形态：
# 一个跑在沙箱里的程序，用平台给它的、权限不超过其所有者的凭据，调平台。
#
# 这类调用在 SRCOS 的审计里记为「这个实例做的」（actor 带 instance），
# 与「某个人做的」可分。
instance_credential <- function() {
  api <- Sys.getenv("SRCOS_API", "")
  tok_file <- file.path(Sys.getenv("HOME"), ".srcos", "agent-token")

  if (identical(api, "")) {
    return(list(ok = FALSE, note = "SRCOS_API 未设置（这台机器上没有已知的网关）"))
  }
  if (!file.exists(tok_file)) {
    return(list(ok = FALSE, note = "没有实例凭据（tool.yaml 未声明 agent:）"))
  }
  tok <- trimws(readLines(tok_file, n = 1L, warn = FALSE))

  h <- curl::new_handle()
  curl::handle_setheaders(h, Authorization = paste("Bearer", tok))
  res <- tryCatch(
    curl::curl_fetch_memory(paste0(api, "/tools"), handle = h),
    error = function(e) NULL
  )
  if (is.null(res)) {
    return(list(ok = FALSE, note = "调用 SRCOS 失败（网关没在跑？）"))
  }
  if (res$status_code != 200L) {
    return(list(ok = FALSE, note = paste0("SRCOS 拒绝了这个凭据：HTTP ", res$status_code)))
  }
  tools <- jsonlite::fromJSON(rawToChar(res$content), simplifyVector = FALSE)$tools
  ids <- vapply(tools, function(t) t$id, character(1))
  list(
    ok = TRUE,
    note = paste0(
      "实例凭据可用（只读）——SRCOS 回了 ", length(ids), " 个工具：",
      paste(ids, collapse = ", ")
    )
  )
}

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
      verbatimTextOutput("who"),
      h4("实例身份（A1）"),
      verbatimTextOutput("cred")
    )
  )
)

server <- function(input, output, session) {
  # 启动时用一次实例凭据；结果显示出来，也就会出现在网关的审计里。
  cred <- instance_credential()

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

  output$cred <- renderText(cred$note)
}

shinyApp(ui, server)
