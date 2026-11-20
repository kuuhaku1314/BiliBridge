const hostName = "com.bilibridge.desktop";
const cookieURL = "https://api.bilibili.com/";
const names = ["SESSDATA", "buvid3", "buvid4"];
const videoPage = /^\/(video\/BV[0-9A-Za-z]{10}|bangumi\/play\/ep[0-9]+)\/?$/;
const liveRoomPage = /^\/[1-9][0-9]{0,18}\/?$/;
const qualities = new Set([6, 16, 32, 64, 74, 80, 112, 116, 120, 125, 126, 127]);
const codecs = new Set([7, 12, 13]);

function isLoginPage(page) {
  return page.origin === "https://www.bilibili.com" && page.pathname === "/" &&
    page.searchParams.get("bilibridge_login") === "1";
}

async function storeForTab(tabId) {
  const stores = await chrome.cookies.getAllCookieStores();
  const store = stores.find(entry => entry.tabIds.includes(tabId));
  if (!store) {
    throw new Error("无法定位当前标签页的 B 站登录会话");
  }
  return store.id;
}

async function readCookie(storeId) {
  // getAll filters parent-domain cookies by their canonical domain.
  // A named lookup uses the Bilibili API origin already in our permissions.
  const cookies = await Promise.all(names.map(name =>
    chrome.cookies.get({url: cookieURL, name, storeId})));
  return cookies.filter(cookie => cookie?.value && !cookie.partitionKey)
    .map(cookie => `${cookie.name}=${cookie.value}`).join("; ");
}

async function syncLogin(storeId) {
  const cookie = await readCookie(storeId);
  if (!cookie.startsWith("SESSDATA=")) {
    return {ok: false, error: "请先在网页中登录 B 站"};
  }
  return chrome.runtime.sendNativeMessage(hostName, {action: "login", cookie});
}

// Login can finish inside the current page without a navigation.
chrome.cookies.onChanged.addListener(({cookie, removed}) => {
  if (removed || cookie.name !== "SESSDATA" || cookie.partitionKey ||
      !["bilibili.com", "api.bilibili.com"].includes(cookie.domain.replace(/^\./, ""))) {
    return;
  }
  (async () => {
    const [tabs, stores] = await Promise.all([
      chrome.tabs.query({url: "https://www.bilibili.com/*"}),
      chrome.cookies.getAllCookieStores()
    ]);
    const store = stores.find(entry => entry.id === cookie.storeId);
    if (store && tabs.some(tab => store.tabIds.includes(tab.id) && tab.url && isLoginPage(new URL(tab.url)))) {
      await syncLogin(store.id);
    }
  })().catch(() => {});
});

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (sender.id !== chrome.runtime.id || sender.frameId !== 0 || !sender.tab || !sender.url ||
      !["play", "sync", "login"].includes(message.action)) {
    return false;
  }
  const page = new URL(sender.url);
  if (message.action === "login" && !isLoginPage(page)) {
    return false;
  }
  const liveRoom = page.origin === "https://live.bilibili.com" && liveRoomPage.test(page.pathname);
  if (!liveRoom && (page.origin !== "https://www.bilibili.com" ||
      (message.action === "play" && !videoPage.test(page.pathname)))) {
    return false;
  }
  const ref = new URL(page.pathname, page.origin);
  const part = page.searchParams.get("p");
  if (!liveRoom && part && /^[1-9][0-9]{0,5}$/.test(part)) {
    ref.searchParams.set("p", part);
  }
  (async () => {
    try {
      const storeId = await storeForTab(sender.tab.id);
      if (message.action === "login") {
        sendResponse(await syncLogin(storeId));
        return;
      }
      const cookie = await readCookie(storeId);
      sendResponse(await chrome.runtime.sendNativeMessage(hostName, {
        action: message.action,
        ref: ref.href,
        at: Number.isFinite(message.at) ? Math.max(0, message.at) : 0,
        quality: qualities.has(message.quality) ? message.quality : 0,
        codec: codecs.has(message.codec) ? message.codec : 0,
        cookie
      }));
    } catch (error) {
      const detail = String(error.message || error);
      const missingHost = /native messaging host|specified native|receiving end/i.test(detail);
      sendResponse({ok: false, error: missingHost ? "请先打开 BiliBridge 托盘程序" : detail});
    }
  })();
  return true;
});
