(() => {
    try {
      const out = (() => { __RESOLVE__ })();
      if (!out?.ok) return { error: out?.error ?? "resolve-failed" };
      const inst = self.__ybInstance;
      const h38 = inst.getLocalQimei36()?.h38 ||
        localStorage.getItem("_qimei_h38") || "";
      if (!h38 || h38.length !== 38) return { error: "invalid-h38", h38 };
      const ts = Date.now();
      const params = "h38=" + h38 + "&timestamp=" + ts + "&platform=web";
      const uskey = inst.getUSKeySync(__APP_ID__, h38, params);
      if (!uskey) return { error: "empty-uskey" };
      return {
        h38,
        ts,
        uskey,
        deviceId: localStorage.getItem("_qimei_uuid42") || "",
      };
    } catch (e) {
      return { error: String(e) };
    }
  })()
