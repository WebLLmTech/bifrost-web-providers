try {
    if (self.__ybInstance) return { ok: true, cached: true };
    const wr = (() => { let r; try { self.webpackChunk_N_E.push([["__yb_resolve_" + Date.now()], {}, q => { r = q; }]); } catch (_) {} return r; })();
    if (!wr) return { error: "no-webpack" };
    const APP_KEY = __APP_KEY__;
    const looksValid = (inst) => !!inst &&
      typeof inst.getUSKeySync === "function" &&
      typeof inst.getLocalQimei36 === "function";
    const fromExport = (mod) => {
      if (!mod) return null;
      if (typeof mod.I5 === "function") {
        try {
          const inst = mod.I5(APP_KEY);
          if (looksValid(inst)) return inst;
        } catch (_) { /* fall through */ }
      }
      return null;
    };
    const tryFactory = (fn) => {
      if (typeof fn !== "function") return null;
      for (const make of [() => fn(APP_KEY), () => new fn(APP_KEY)]) {
        try {
          const inst = make();
          if (looksValid(inst)) return inst;
        } catch (_) { /* next */ }
      }
      return null;
    };
    const scanModule = (mod) => {
      if (!mod || typeof mod !== "object") return null;
      const preferred = fromExport(mod);
      if (preferred) return preferred;
      for (const value of Object.values(mod)) {
        const inst = tryFactory(value);
        if (inst) return inst;
      }
      return looksValid(mod) ? mod : null;
    };
    for (const id of __MODULE_IDS__) {
      try {
        const inst = scanModule(wr(id));
        if (inst) {
          self.__ybInstance = inst;
          return { ok: true, moduleId: id };
        }
      } catch (_) { /* next known id */ }
    }
    for (const id of Object.keys(wr.m)) {
      try {
        if (!String(wr.m[id]).includes("getUSKeySync")) continue;
        const inst = scanModule(wr(id));
        if (inst) {
          self.__ybInstance = inst;
          return { ok: true, moduleId: id };
        }
      } catch (_) { /* keep scanning */ }
    }
    return { error: "qimei-module-not-found" };
  } catch (e) {
    return { error: String(e) };
  }
