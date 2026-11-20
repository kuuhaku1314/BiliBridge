const videoPage = /^\/(video\/BV[0-9A-Za-z]{10}|bangumi\/play\/ep[0-9]+)\/?$/;
const liveRoomPage = /^\/[1-9][0-9]{0,18}\/?$/;
const liveOrigin = location.origin === "https://live.bilibili.com";
let pendingFrame = 0;
let toolbar;
let toolbarPlayer;

function readPlayerSource(video) {
  if (!video) return {};
  let actual;
  const sourceEvent = event => { actual = event.detail; };
  video.addEventListener("bilibridge-source", sourceEvent, {once: true});
  video.dispatchEvent(new Event("bilibridge-read-source", {bubbles: true}));
  video.removeEventListener("bilibridge-source", sourceEvent);
  return actual || {};
}

function selectedSource(player, video) {
  if (!video) return {};
  const actual = readPlayerSource(video);
  const entry = player.querySelector(".bpx-player-ctrl-quality-menu-item.bpx-state-active[data-value], " +
    ".bpx-player-ctrl-quality-menu-item[aria-selected='true'][data-value], .squirtle-select-item.active[data-value]");
  const label = (player.querySelector(".bpx-player-ctrl-quality-result, .squirtle-select-text")?.textContent || "")
    .toUpperCase().replace(/\s/g, "");
  const labels = [["8K", 127], ["杜比视界", 126], ["HDR", 125], ["4K", 120], ["1080P60", 116],
    ["1080P高码率", 112], ["1080P+", 112], ["1080P", 80], ["720P60", 74], ["720P", 64],
    ["480P", 32], ["360P", 16], ["240P", 6]];
  const shortEdge = Math.min(video.videoWidth, video.videoHeight);
  const dimensions = [[4320, 127], [2160, 120], [1080, 80], [720, 64], [480, 32], [360, 16], [1, 6]];
  const quality = Number(entry?.dataset.value) || actual?.quality || labels.find(([name]) => label.includes(name))?.[1] ||
    dimensions.find(([edge]) => shortEdge >= edge)?.[1];
  let codec = actual?.codec;
  if (!codec) {
    try {
      codec = {1: 12, 2: 7, 3: 13}[localStorage.getItem("bilibili_player_codec_prefer_type")];
    } catch {
      // The player can choose its codec automatically.
    }
  }
  return {quality, codec};
}

function selectedLiveQuality(player, video) {
  const {quality} = readPlayerSource(video);
  if (quality) return quality;
  const label = player.querySelector(".quality-wrap .selected-qn")?.textContent || "";
  const labels = [["杜比", 30000], ["4K", 20000], ["2K", 15000], ["原画", 10000], ["蓝光", 400],
    ["超清", 250], ["高清", 150], ["流畅", 80]];
  return labels.find(([name]) => label.toUpperCase().includes(name))?.[1];
}

function addPotPlayerButton() {
  pendingFrame = 0;
  const live = liveOrigin && liveRoomPage.test(location.pathname);
  const player = live ? document.querySelector("#live-player, .live-player-mounter") :
    (!liveOrigin && videoPage.test(location.pathname) ? document.querySelector(".bpx-player-container") : null);
  if (!player || (!live && !player.querySelector("video"))) {
    toolbar?.remove();
    toolbar = toolbarPlayer = null;
    return;
  }
  const host = live ? document.querySelector(".head-info-section .header-right") :
    player.closest("#playerWrap, #bilibili-player-wrap") || player;
  if (!host) {
    toolbar?.remove();
    toolbar = toolbarPlayer = null;
    return;
  }
  // Keep the server-rendered DOM intact until Bilibili's Vue app takes over.
  if (host.closest("[data-server-rendered]")) return;
  if (toolbarPlayer === player) {
    if (toolbar.parentElement !== host) host.insertBefore(toolbar, live ? host.firstChild : null);
    return;
  }
  toolbar?.remove();
  toolbar = document.createElement("div");
  toolbar.className = "bilibridge-toolbar";
  if (live) toolbar.classList.add("bilibridge-toolbar-live");
  toolbarPlayer = player;
  const button = document.createElement("button");
  const status = document.createElement("output");
  button.type = "button";
  button.textContent = "PotPlayer";
  button.className = "bilibridge-open";
  button.title = live ? "在 PotPlayer 打开直播" : "从当前进度在 PotPlayer 播放";
  status.className = "bilibridge-status";
  status.hidden = true;
  status.setAttribute("role", "status");
  let statusTimer;
  button.addEventListener("click", async event => {
    if (!event.isTrusted) {
      return;
    }
    event.stopPropagation();
    const video = player.querySelector("video");
    const source = live ? {quality: selectedLiveQuality(player, video)} : selectedSource(player, video);
    const pageAtClick = location.href;
    button.disabled = true;
    clearTimeout(statusTimer);
    status.hidden = false;
    status.textContent = "正在打开…";
    try {
      const result = await chrome.runtime.sendMessage({
        action: "play", ref: pageAtClick, at: live ? 0 : (video?.currentTime || 0), ...source
      });
      if (!result?.ok) {
        status.textContent = result?.error || "BiliBridge 未返回播放结果";
        return;
      }
      if (location.href === pageAtClick) {
        video?.pause();
      }
      const codec = {7: "H.264", 12: "HEVC", 13: "AV1"}[result.codec];
      const format = result.qualityLabel || (result.height > 0 ? `${result.height}P` : "");
      const adjusted = (source.quality && result.quality && source.quality !== result.quality) ||
        (source.codec && result.codec && source.codec !== result.codec);
      status.textContent = [adjusted ? "已打开（已调整播放源）" : "已打开", format, codec].filter(Boolean).join(" · ");
    } catch (error) {
      status.textContent = error.message || "无法连接 BiliBridge";
    } finally {
      button.disabled = false;
      statusTimer = setTimeout(() => { status.hidden = true; }, 5500);
    }
  });
  toolbar.append(status, button);
  host.insertBefore(toolbar, live ? host.firstChild : null);
}

new MutationObserver(() => {
  if (!pendingFrame) {
    pendingFrame = requestAnimationFrame(addPotPlayerButton);
  }
}).observe(document.documentElement, {
  childList: true, subtree: true, attributes: true, attributeFilter: ["data-server-rendered"]
});
addPotPlayerButton();
if (!liveOrigin || liveRoomPage.test(location.pathname)) {
  const login = location.origin === "https://www.bilibili.com" && location.pathname === "/" &&
    new URLSearchParams(location.search).get("bilibridge_login") === "1";
  chrome.runtime.sendMessage({action: login ? "login" : "sync"}).catch(() => {});
}
