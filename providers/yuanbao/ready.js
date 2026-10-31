(() => {
  const out = (() => { __RESOLVE__ })();
  if (!out?.ok) return false;
  try {
    const h38 = self.__ybInstance.getLocalQimei36()?.h38 ||
      localStorage.getItem("_qimei_h38") || "";
    return typeof h38 === "string" && h38.length === 38;
  } catch (_) {
    return false;
  }
})()
