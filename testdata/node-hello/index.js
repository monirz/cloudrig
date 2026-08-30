exports.handler = (req, res) => {
  const name = req.query.name || req.body?.name || 'cloudrig';
  console.log(`${req.method} ${req.url} -> ${name}`);
  res.send(`Hello, ${name}!`);
};
