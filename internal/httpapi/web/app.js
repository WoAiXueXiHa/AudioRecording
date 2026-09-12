"use strict";
const $ = (id) => document.getElementById(id),
  names = {
    pending: "排队中",
    transcribing: "转写中",
    summarizing: "生成摘要",
    done: "已完成",
    failed: "处理失败",
  };
let page = 1,
  total = 0,
  selected = null,
  loading = false,
  uploading = false,
  detailId = null,
  detailVersion = 0,
  toastTimer;
function toast(text) {
  $("message").textContent = text;
  $("message").hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("message").hidden = true), 6000);
}
async function api(path, options = {}) {
  const r = await fetch(path, {
    ...options,
    signal: AbortSignal.timeout(90000),
  });
  if (r.status === 204) return null;
  const data = await r.json();
  if (!r.ok) throw new Error(data.error?.message || `请求失败 (${r.status})`);
  return data;
}
function el(tag, text, cls) {
  const n = document.createElement(tag);
  if (text !== undefined) n.textContent = text;
  if (cls) n.className = cls;
  return n;
}
function button(text, fn, cls = "quiet") {
  const b = el("button", text, cls);
  b.type = "button";
  b.onclick = async () => {
    b.disabled = true;
    try {
      await fn();
    } catch (e) {
      toast(e.message);
    } finally {
      b.disabled = false;
    }
  };
  return b;
}
function choose(file) {
  if (uploading) return;
  if (!file) return;
  if (
    !/\.(mp3|wav|m4a|aac)$/i.test(file.name) ||
    file.size === 0 ||
    file.size > 50 * 1024 * 1024
  ) {
    toast("请选择非空的 MP3 / WAV / M4A / AAC 文件，大小不超过 50 MiB");
    return;
  }
  selected = file;
  $("file-label").textContent = file.name;
}
$("file").onchange = (e) => choose(e.target.files[0]);
for (const event of ["dragenter", "dragover"])
  $("drop").addEventListener(event, (e) => {
    e.preventDefault();
    $("drop").classList.add("drag");
  });
for (const event of ["dragleave", "drop"])
  $("drop").addEventListener(event, (e) => {
    e.preventDefault();
    $("drop").classList.remove("drag");
    if (event === "drop") choose(e.dataTransfer.files[0]);
  });
$("upload-form").onsubmit = async (e) => {
  e.preventDefault();
  if (uploading) return;
  if (!selected) {
    toast("请先选择音频文件");
    return;
  }
  uploading = true;
  $("upload-button").disabled = true;
  $("file").disabled = true;
  $("upload-button").textContent = "正在上传…";
  try {
    const form = new FormData();
    form.append("file", selected);
    await api("/v1/recordings", { method: "POST", body: form });
    selected = null;
    $("file").value = "";
    $("file-label").textContent = "点击选择或拖入音频";
    page = 1;
    toast("上传成功，任务已加入处理队列");
    await refresh();
  } catch (e) {
    toast("上传未确认成功，请先刷新列表核对，再决定是否重传。" + e.message);
  } finally {
    uploading = false;
    $("file").disabled = false;
    $("upload-button").disabled = false;
    $("upload-button").textContent = "上传并生成摘要 ↗";
  }
};
async function refresh() {
  if (loading) return;
  loading = true;
  try {
    const data = await api(`/v1/recordings?page=${page}&page_size=6`);
    total = data.total;
    if (page > 1 && data.items.length === 0) {
      page--;
      loading = false;
      return refresh();
    }
    $("count").textContent = total;
    $("page-label").textContent =
      `第 ${page} / ${Math.max(1, Math.ceil(total / 6))} 页`;
    $("prev").disabled = page <= 1;
    $("next").disabled = page * 6 >= total;
    $("list").replaceChildren();
    for (const r of data.items) {
      const row = el("div", undefined, "row"),
        body = el("div", undefined, "row-main");
      body.append(
        button(r.original_filename, () => showDetail(r.id), "filename"),
        el(
          "div",
          `${new Date(r.created_at).toLocaleString("zh-CN", { hour12: false })} · ${(r.file_size / 1024 / 1024).toFixed(2)} MB`,
          "meta",
        ),
      );
      const actions = el("div", undefined, "actions");
      actions.append(button("查看", () => showDetail(r.id)));
      if (r.status === "failed")
        actions.append(
          button("重试", async () => {
            await api(`/v1/tasks/${r.task_id}/retry`, { method: "POST" });
            toast("已重新加入队列");
            await refresh();
          }),
        );
      if (["done", "failed"].includes(r.status))
        actions.append(
          button(
            "删除",
            async () => {
              if (!confirm(`确定删除「${r.original_filename}」及其全部结果？`))
                return;
              await api(`/v1/recordings/${r.id}`, { method: "DELETE" });
              if (detailId === r.id) {
                $("detail").close();
                detailId = null;
              }
              await refresh();
            },
            "quiet danger",
          ),
        );
      row.append(
        el("div", "≋", "audio-icon"),
        body,
        el(
          "span",
          names[r.status] || "未知状态",
          `badge ${Object.hasOwn(names, r.status) ? r.status : ""}`,
        ),
        actions,
      );
      $("list").append(row);
    }
    if (!data.items.length)
      $("list").append(
        el("div", "还没有录音，从左侧上传第一段声音吧。", "empty"),
      );
    if (detailId && $("detail").open) await showDetail(detailId, false);
  } catch (e) {
    toast("加载失败：" + e.message);
    if (!$("list").querySelector(".row"))
      $("list").replaceChildren(
        el("div", "暂时无法加载，请点击刷新重试。", "empty"),
      );
  } finally {
    loading = false;
  }
}
async function showDetail(id, open = true) {
  const version = ++detailVersion;
  detailId = id;
  if (open) {
    $("detail-title").textContent = "正在加载…";
    $("detail-body").replaceChildren();
    if (!$("detail").open) $("detail").showModal();
  }
  const r = await api(`/v1/recordings/${id}`);
  let task = null;
  if (r.status === "failed" && r.task_id)
    task = await api(`/v1/tasks/${r.task_id}`);
  if (version !== detailVersion || detailId !== id) return;
  $("detail-title").textContent = r.original_filename;
  const body = $("detail-body");
  body.replaceChildren(
    el("span", names[r.status] || "未知状态", `badge ${r.status}`),
  );
  if (task)
    body.append(
      el(
        "p",
        `失败原因：${task.error_message || "未知"} (${task.error_code || "unknown"})。关闭详情后可点击重试。`,
      ),
    );
  if (!["done", "failed"].includes(r.status))
    body.append(el("p", "任务正在处理中，此窗口会自动更新。"));
  for (const [title, value] of [
    ["一句话摘要", r.summary],
    ["关键要点", r.key_points],
    ["待办事项", r.todos],
    ["转写文本（模拟）", r.transcript],
  ]) {
    if (value === null || value === undefined) continue;
    body.append(el("h3", title));
    if (Array.isArray(value)) {
      const list = el("ul");
      value.forEach((x) => list.append(el("li", x)));
      body.append(value.length ? list : el("p", "暂无"));
    } else body.append(el("p", value));
  }
}
$("close").onclick = () => $("detail").close();
$("detail").addEventListener("close", () => {
  detailId = null;
  detailVersion++;
});
$("refresh").onclick = refresh;
$("prev").onclick = () => {
  if (!loading && page > 1) {
    page--;
    refresh();
  }
};
$("next").onclick = () => {
  if (!loading && page * 6 < total) {
    page++;
    refresh();
  }
};
setInterval(() => {
  if (!document.hidden) refresh();
}, 4000);
refresh();
