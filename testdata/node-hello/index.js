exports.handler = (req, res) => {
  res.send(`Hello, ${req.query.name || req.body?.name || 'cloudrig'}!`);
};
