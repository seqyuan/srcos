# Shiny 应用：首次使用时由 SRCOS 从 tool.yaml 的 workspace.init_from 拷进
# /workspace。之后它就是**你的**文件 —— 改完重启实例即可看到效果。

library(shiny)

# 工具的 interface 参数以 SRCOS_PARAM_<大写名> 注入；管理端启动不带参数时
# 回落到 app.R 里的默认值。
title <- Sys.getenv("SRCOS_PARAM_TITLE", "SRCOS Shiny Demo")
if (identical(title, "")) title <- "SRCOS Shiny Demo"

# ── 托管 agent 的实例身份（ADR-025）───────────────────────────────────────
# tool.yaml 声明了 `agent: { mcp: [read, submit], tools: [hello-fanout] }`，
# 于是 SRCOS 在实例启动时签发一枚实例凭据到 $HOME/.srcos/agent-token：
# scope 与白名单**来自工具声明**，所以它是所有者权限的子集。
#
# 下面的两个调用就是「托管 agent」的最小形态：
#   · 读  —— 用凭据列自己能看到的工具
#   · 写  —— 用凭据提交一个任务（白名单只放了 hello-fanout）
# 两者在 SRCOS 的审计/日志里都记为「这个实例做的」，而不是「某个人做的」。

instance_token <- function() {
  tok_file <- file.path(Sys.getenv("HOME"), ".srcos", "agent-token")
  if (!file.exists(tok_file)) return(NULL)
  trimws(readLines(tok_file, n = 1L, warn = FALSE))
}

# srcos_call 用实例凭据打 SRCOS 的 REST API，返回状态码与响应体。
srcos_call <- function(path, body = NULL) {
  api <- Sys.getenv("SRCOS_API", "")
  if (identical(api, "")) {
    return(list(status = 0L, body = "SRCOS_API 未设置（这台机器上没有已知的网关）"))
  }
  tok <- instance_token()
  if (is.null(tok)) {
    return(list(status = 0L, body = "没有实例凭据（tool.yaml 未声明 agent:）"))
  }

  h <- curl::new_handle()
  headers <- list(Authorization = paste("Bearer", tok))
  if (!is.null(body)) {
    headers[["Content-Type"]] <- "application/json"
    curl::handle_setopt(h, postfields = jsonlite::toJSON(body, auto_unbox = TRUE))
  }
  curl::handle_setheaders(h, .list = headers)

  res <- tryCatch(
    curl::curl_fetch_memory(paste0(api, path), handle = h),
    error = function(e) NULL
  )
  if (is.null(res)) return(list(status = 0L, body = "调用 SRCOS 失败（网关没在跑？）"))
  list(status = res$status_code, body = rawToChar(res$content))
}

# 启动时读一次：证明凭据可用，也把结果留在页面上。
visible_tools <- function() {
  r <- srcos_call("/tools")
  if (r$status != 200L) return(paste0("读工具目录失败：HTTP ", r$status, " ", r$body))
  ids <- vapply(jsonlite::fromJSON(r$body, simplifyVector = FALSE)$tools,
                function(t) t$id, character(1))
  paste0("实例凭据可用（read）——SRCOS 回了 ", length(ids), " 个工具：", paste(ids, collapse = ", "))
}

ui <- fluidPage(
  titlePanel(title),
  sidebarLayout(
    sidebarPanel(
      sliderInput("n", "样本数", min = 2, max = 20, value = 3),
      selectInput("dist", "分布", c("正态" = "norm", "均匀" = "unif")),
      hr(),
      helpText("这个实例跑在 SRCOS 的沙箱里：只有一个工作区（/workspace）可写，",
               "端口由平台分配。"),
      helpText("下面的按钮用**实例凭据**提交一个真实任务；白名单里只有",
               " hello-fanout，所以该凭据只能提交它。"),
      actionButton("submit", "提交一个任务（hello-fanout）"),
      actionButton("refresh", "刷新状态")
    ),
    mainPanel(
      plotOutput("hist"),
      verbatimTextOutput("who"),
      h4("实例身份（A1）"),
      verbatimTextOutput("cred"),
      h4("用实例凭据执行（submit）"),
      verbatimTextOutput("job")
    )
  )
)

server <- function(input, output, session) {
  cred_line <- visible_tools()
  job_msg <- reactiveVal("还没有提交。")
  last_instance <- reactiveVal(NULL)

  # 提交：幂等键按这次点击生成，所以重试**同一次**提交不会起两个任务
  # （A2）—— 换一次尝试要换 key，那是调用方的语义。
  observeEvent(input$submit, {
    key <- paste0("shiny-", format(Sys.time(), "%Y%m%d%H%M%S"), "-", sample.int(1e6, 1))
    r <- srcos_call("/jobs", list(
      tool = "hello-fanout",
      name = "from shiny (instance credential)",
      # samples 是 interface 里的 type: string（逗号分隔），不是数组：
      # 长度 >1 的向量会被 jsonlite 序列化成数组（平台会拒）。
      params = list(samples = paste(sprintf("S%02d", seq_len(input$n)), collapse = ",")),
      # outputs 是数组：jsonlite 的 auto_unbox 会把长度 1 的字符串拆成字符串，
      # 所以裹一层 list() 保住数组形状（平台会直接报 invalid JSON）。
      outputs = list("/workspace/out"),
      idempotencyKey = key
    ))
    if (r$status == 201L || r$status == 200L) {
      res <- jsonlite::fromJSON(r$body, simplifyVector = FALSE)
      last_instance(res$instanceId)
      job_msg(paste0(
        "已提交（HTTP ", r$status, "）\n  job      ", res$jobId,
        "\n  instance ", res$instanceId,
        "\n  state    ", res$state
      ))
    } else {
      job_msg(paste0("被拒：HTTP ", r$status, "\n", r$body))
    }
  })

  # 状态：列自己的实例，找刚提交的那个。
  observeEvent(input$refresh, {
    id <- last_instance()
    if (is.null(id)) { job_msg("还没有提交。"); return() }
    r <- srcos_call("/jobs")
    if (r$status != 200L) { job_msg(paste0("查询失败：HTTP ", r$status, " ", r$body)); return() }
    jobs <- jsonlite::fromJSON(r$body, simplifyVector = FALSE)$jobs
    hit <- Filter(function(j) identical(j$id, id), jobs)
    job_msg(if (length(hit) == 0) {
      paste0(id, "：不在列表里（还没写记录？）")
    } else {
      paste0(id, " → state=", hit[[1]]$state)
    })
  })

  samples <- reactive({
    switch(input$dist, norm = rnorm(input$n), unif = runif(input$n))
  })

  output$hist <- renderPlot({
    hist(samples(), main = paste("n =", input$n, "·", input$dist),
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

  output$cred <- renderText(cred_line)
  output$job <- renderText(job_msg())
}

shinyApp(ui, server)
