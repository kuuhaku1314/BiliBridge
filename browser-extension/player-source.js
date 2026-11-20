(() => {
  const sources = new Map();
  const codecs = new WeakMap();
  const owners = new WeakMap();

  function rememberCodec(source, type) {
    const match = /\b(avc[13]|hev1|hvc1|av01)\./i.exec(type);
    if (!match) return;
    const codec = match[1].toLowerCase();
    codecs.set(source, codec.startsWith("avc") ? 7 : codec === "av01" ? 13 : 12);
  }

  const createObjectURL = URL.createObjectURL;
  URL.createObjectURL = function (object) {
    const url = createObjectURL.apply(this, arguments);
    if (object instanceof MediaSource) {
      sources.set(url, object);
      if (sources.size > 16) sources.delete(sources.keys().next().value);
    }
    return url;
  };

  const addSourceBuffer = MediaSource.prototype.addSourceBuffer;
  MediaSource.prototype.addSourceBuffer = function (type) {
    const buffer = addSourceBuffer.apply(this, arguments);
    owners.set(buffer, this);
    rememberCodec(this, type);
    return buffer;
  };

  const changeType = SourceBuffer.prototype.changeType;
  if (changeType) {
    SourceBuffer.prototype.changeType = function (type) {
      const result = changeType.apply(this, arguments);
      const source = owners.get(this);
      if (source) rememberCodec(source, type);
      return result;
    };
  }

  document.addEventListener("bilibridge-read-source", event => {
    const video = event.target;
    if (!(video instanceof HTMLVideoElement)) return;
    let quality;
    try {
      if (location.hostname === "live.bilibili.com") {
        quality = Number.parseInt(window.livePlayer?.getPlayerInfo?.()?.quality, 10);
      } else {
        const selected = window.player?.getQulity?.() ?? window.player?.getQuality?.();
        quality = selected?.realQ || selected?.nowQ || selected;
      }
    } catch {
      // The visible quality menu is also available to the content script.
    }
    video.dispatchEvent(new CustomEvent("bilibridge-source", {detail: {
      quality: Number(quality) || 0,
      codec: codecs.get(sources.get(video.currentSrc)) || 0
    }}));
  }, true);
})();
